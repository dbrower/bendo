package blobcache

import (
	"encoding/json"
	"io"
	"log"
	"sort"
	"sync"
	"time"

	raven "github.com/getsentry/raven-go"

	"github.com/ndlib/bendo/store"
)

// A TimeBased cache will keep items for a fixed length of time after their
// last access. If they are accessed again in that period of time, their
// expiration clock is reset. Items whose expiration clock expires will be
// removed from the cache.
//
// The total amount of storage used by this cache will vary over time, and may
// grow without bound.
type TimeBased struct {
	// place to put cached content
	s store.Store

	// length of time to keep accessed items around
	ttl time.Duration

	// close this channel to cancel the background goroutine
	done chan struct{}

	m sync.RWMutex // protects everything to the --- below

	// total size in bytes used by the cache. If 0 then we have not calculated
	// the total size of the cache yet.
	size int64

	// cache items hashed by key
	items map[string]timeEntry

	// set of keys that have a Put() started on them, but not closed yet.
	pending map[string]struct{}

	//--- end section protected by mutex m
	// acquire order: expireM and then m
	expireM sync.Mutex // protects the items below

	// index of when to check items for expiration
	expireList []timeEntry
}

// indexFilename is the key we use to persist our list of expiration times
// between executions. It is only a suggestion, and does not need to be
// present.
//
// Since this file is stored in the cache space, we should munge key names so
// none collide with this file. e.g. map any user provided key k beginning with
// indexFilename to k + "-" or something.
const indexFilename = "ITEM-LIST"

type timeEntry struct {
	Key     string
	Size    int64
	Expires time.Time
}

// NewTime returns a new time-based cache using s as the backing store and with
// items having a time-to-live of duration d.
func NewTime(s store.Store, d time.Duration) *TimeBased {
	te := &TimeBased{
		s:       s,
		ttl:     d,
		items:   make(map[string]timeEntry),
		pending: make(map[string]struct{}),
		done:    make(chan struct{}),
	}
	go te.background()
	return te
}

// Stop will stop the background goroutine that was spawned in NewTime().
//
// (Is there a better name than `Stop`?)
func (te *TimeBased) Stop() {
	close(te.done)
}

// Contains returns true if the given key is in the cache when the function is
// called. The key may be removed in between calling Contains() and Get().
func (te *TimeBased) Contains(key string) bool {
	te.m.RLock()
	_, result := te.items[key]
	te.m.RUnlock()
	return result
}

// Get returns a reader for reading the content stored at the given key.
func (te *TimeBased) Get(key string) (store.ReadAtCloser, int64, error) {
	te.m.Lock()
	defer te.m.Unlock()
	item, exists := te.items[key]
	if !exists {
		return nil, 0, nil
	}
	// update the expires time
	item.Expires = time.Now().Add(te.ttl)
	te.items[key] = item
	rac, size, err := te.s.Open(key)
	if err != nil {
		// Something happened getting the item. Assume it is bad and just remove
		// it from our list
		te.delete(key)
	}
	return rac, size, err
}

// Put returns a writer for saving the content of the given key. The item is
// added to the cache when the writer is closed. The error `ErrPutPending` is
// returned if someone else is currently saving content to the key. If the item
// is already in the cache, it is deleted first.
func (te *TimeBased) Put(key string) (io.WriteCloser, error) {
	te.m.Lock()
	_, exists := te.pending[key]
	te.pending[key] = struct{}{} // doesn't hurt since we already know exists
	te.m.Unlock()
	if exists {
		return nil, ErrPutPending
	}
	w, err := te.s.Create(key)
	// special case situation where the key already exists to try again after
	// deleting the key.
	// since we passed the pending check, there are no open Puts on that key
	if err == store.ErrKeyExists {
		te.s.Delete(key)
		w, err = te.s.Create(key)
	}
	if err != nil {
		te.unpending(key)
		return nil, err
	}
	return &writer{parent: te, key: key, w: w}, nil
}

func (te *TimeBased) addEntry(entry timeEntry) {
	te.expireM.Lock()
	defer te.expireM.Unlock()
	te.m.Lock()
	defer te.m.Unlock()

	entry.Expires = time.Now().Add(te.ttl)
	te.items[entry.Key] = entry
	te.expireList = append(te.expireList, entry)
	te.size += entry.Size
}

func (te *TimeBased) save(w *writer) {
	te.addEntry(timeEntry{Key: w.key, Size: w.size})
	te.m.Lock()
	delete(te.pending, w.key)
	te.m.Unlock()
}

func (te *TimeBased) unpending(key string) {
	te.m.Lock()
	defer te.m.Unlock()
	delete(te.pending, key)
}

// discard is used by a child Writer object to signal this item should be
// forgotten about.
func (te *TimeBased) discard(w *writer) {
	te.unpending(w.key)
}

// reserve is needed for `saver` interface.
// we add the size of the new object all at once in save().
func (te *TimeBased) reserve(int64) error { return nil }

// Delete removes the given item from the cache.
func (te *TimeBased) Delete(key string) error {
	te.m.Lock()
	err := te.delete(key)
	te.m.Unlock()
	te.writeIndexFile()
	return err
}

// delete removes an item from the cache. It assumes the lock m is already
// held. Use Delete() if you are not currently holding the lock.
func (te *TimeBased) delete(key string) error {
	item, ok := te.items[key]
	if !ok {
		return nil
	}
	te.size -= item.Size
	delete(te.items, key)
	return te.s.Delete(key)
}

// Size returns the amount of storage currently used by the cache in bytes.
func (te *TimeBased) Size() int64 {
	te.m.RLock()
	defer te.m.RUnlock()
	return te.size
}

// MaxSize always returns 0 since there is no size limit for a TimeBased cache.
func (te *TimeBased) MaxSize() int64 {
	return 0
}

// background is the goroutine that manages saving the index file and purging
// expired keys.
func (te *TimeBased) background() {
	// this is done by Scan().
	// one concern is if the background process runs before Scan() is called,
	// it will replace the item-list file with an empty one. On the other hand
	// it is easier to test if the background process does not do a Scan()
	//te.readIndexFile()
	//te.scanstore()

	// Figure out how often to check for expired keys and save the index file.
	// Duration is either 1/4 of the TTL or once a day, whichever is shorter.
	// These amounts are arbitrary, feel free to adjust.
	d := te.ttl / 4
	if d > 24*time.Hour {
		d = 24 * time.Hour
	}
	for {
		select {
		case <-te.done:
			return
		case <-time.After(d):
		}
		te.expireKeys()
		te.writeIndexFile()
	}
}

// expireKeys will attempt to remove all keys who have expired. Only expired
// keys will be removed, but some expired keys might be missed. This should be
// called on some regular basis.
//
// expireKeys holds the expireM mutex while it runs, so only one can run at a
// time.
func (te *TimeBased) expireKeys() {
	// The expireList is kept so we do not need to scan every item in the cache
	// to figure out what needs to be removed. However, since items in list are
	// not updated after being added, their expire times may have changed. We
	// only need to check items that the list thinks are expired since item
	// expire times only move forward in time (hopefully!).
	te.expireM.Lock()
	defer te.expireM.Unlock()

	now := time.Now()
	sort.Sort(byExpires(te.expireList))
	for i, item := range te.expireList {
		if item.Expires.After(now) {
			te.expireList = te.expireList[i:]
			break
		}
		// get the actual item to ensure we don't remove something prematurely
		te.m.Lock()
		item, ok := te.items[item.Key]
		if ok {
			if item.Expires.After(now) {
				// item's expire time has been updated, add to the end of list
				// it will be sorted into the correct position next time.
				te.expireList = append(te.expireList, item)
			} else {
				te.delete(item.Key)
			}
		}
		te.m.Unlock()
	}
}

type byExpires []timeEntry

func (e byExpires) Len() int           { return len(e) }
func (e byExpires) Less(i, j int) bool { return e[i].Expires.Before(e[j].Expires) }
func (e byExpires) Swap(i, j int)      { e[i], e[j] = e[j], e[i] }

func (te *TimeBased) writeIndexFile() {
	te.s.Delete(indexFilename)
	w, err := te.s.Create(indexFilename)
	if err != nil {
		log.Println("Error creating", indexFilename, ":", err)
		raven.CaptureError(err, nil)
		return
	}
	enc := json.NewEncoder(w)
	te.m.RLock()
	err = enc.Encode(te.items)
	te.m.RUnlock()
	if err != nil {
		log.Println("Error writing", indexFilename, ":", err)
		raven.CaptureError(err, nil)
	}
	w.Close()
}

func (te *TimeBased) readIndexFile() {
	rac, _, err := te.s.Open(indexFilename)
	if err != nil {
		// If the index file does not already exist, it will generate an error.
		// Is is not a problem, but we log the error anyway.
		log.Println("Error opening", indexFilename, ":", err)
		raven.CaptureError(err, nil)
		return
	}
	dec := json.NewDecoder(store.NewReader(rac))
	var items map[string]timeEntry
	dec.Decode(&items)
	rac.Close()

	// insert the new items into the map
	te.expireM.Lock()
	defer te.expireM.Unlock()
	te.m.Lock()
	defer te.m.Unlock()
	for _, v := range items {
		// NOTE: calling addEntry will reset the timestamps. so we inline the
		// relevant code here.
		if _, exists := te.items[v.Key]; !exists {
			te.items[v.Key] = v
			te.expireList = append(te.expireList, v)
			te.size += v.Size
		}
	}
}

// scan the files currently in the cache and add them if they are not already
// in our index. The added items are given the default expiry time.
func (te *TimeBased) scanstore() {
	for key := range te.s.List() {
		if key == indexFilename || te.Contains(key) {
			continue
		}
		rac, size, err := te.s.Open(key)
		if err != nil {
			continue
		}
		rac.Close()
		te.addEntry(timeEntry{Key: key, Size: size})
	}
}

// Scan will scan the backing store for items and also try to load previous
// expire times from a cache file. An updated index file is saved.
func (te *TimeBased) Scan() {
	te.readIndexFile()
	te.scanstore()
	te.writeIndexFile() // make sure things we just scanned end up in the index
}

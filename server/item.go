package server

import (
	"encoding/hex"
	"errors"
	"expvar"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	raven "github.com/getsentry/raven-go"
	"github.com/julienschmidt/httprouter"
	"golang.org/x/sync/singleflight"

	"github.com/ndlib/bendo/items"
	"github.com/ndlib/bendo/store"
)

var (
	nCacheHit  = expvar.NewInt("cache.hit")
	nCacheMiss = expvar.NewInt("cache.miss")
)

// blobDB are the methods we need to interact with the new item metadata caching.
// This interface is expected to grow as more functionality is moved to the database.
//
// The goal is to remove the original database Cache interface along with its hooks into the
// item package.
type blobDB interface {
	// Look up the metadata for the given item+blob id. Returns error if error encountered.
	// returns nil,nil if the blob was not found in the index.
	FindBlob(item string, blobid int) (*items.Blob, error)

	// Look up blob metadata using an item+version+slot name combo. Returns error if one
	// happened. Returns nil,nil if no such blob is in the index, so a missing item is not an error.
	// The slot name needs to be exact, no wildcard expansion is done.
	// Use version = 0 to refer to the most recent version of the item.
	FindBlobBySlot(item string, version int, slot string) (*items.Blob, error)

	// Index the given item using the given id.
	// (The item id should already be in the item structure. can that parameter be removed?)
	IndexItem(itemid string, item *items.Item) error
}

// SlotHandler handles requests to GET /item/:id/*slot
//                and requests to HEAD /item/:id/*slot
func (s *RESTServer) SlotHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	id := ps.ByName("id")
	// the star parameter in httprouter returns the leading slash
	slot := ps.ByName("slot")[1:]

	item, err := s.Items.Item(id)

	if err != nil {
		switch {
		case err == items.ErrNoStore:
			// if item store use disabled, return 503
			w.WriteHeader(503)
			log.Printf("GET/HEAD /item/%s/%s returns 503 - tape disabled", id, slot)
		case err == items.ErrNoItem:
			w.WriteHeader(404)
		default:
			raven.CaptureError(err, nil)
			log.Println(id, ":", err)
			w.WriteHeader(500)
		}
		fmt.Fprintln(w, err)
		return
	}
	// if we have the empty path, reroute to the item metadata handler
	if slot == "" {
		s.ItemHandler(w, r, ps)
		return
	}
	// slot might have a "@nnn" version prefix
	bid := item.BlobByExtendedSlot(slot)
	if bid == 0 {
		w.WriteHeader(404)
		fmt.Fprintf(w, "Invalid Version")
		return
	}
	w.Header().Set("X-Content-Sha256", hex.EncodeToString(item.Blobs[bid-1].SHA256))
	w.Header().Set("X-Content-Md5", hex.EncodeToString(item.Blobs[bid-1].MD5))
	w.Header().Set("Location", fmt.Sprintf("/item/%s/@blob/%d", id, bid))
	s.getblob(w, r, id, items.BlobID(bid))
}

// getblob will find the given blob, either in the cache or on
// tape, and then send it as a response. If there is an error, it
// will return an error response.
func (s *RESTServer) getblob(w http.ResponseWriter, r *http.Request, id string, bid items.BlobID) {
	// GET requests always cache content. HEAD requests cache content only if
	// the Request-Cache header is passed (with any value)
	docache := r.Method == "GET" || r.Header.Get("Request-Cache") != ""
	key := fmt.Sprintf("%s+%04d", id, bid)
	firsttime := true
retry:
	content, err := s.findContent(key, id, bid, docache)
	if err == items.ErrNoStore {
		w.WriteHeader(503)
		fmt.Fprintln(w, err)
		return
	} else if err == items.ErrDeleted {
		w.WriteHeader(410)
		fmt.Fprintln(w, err)
		return
	} else if _, ok := err.(items.NoBlobError); ok {
		w.WriteHeader(404)
		fmt.Fprintln(w, err)
		return
	} else if err != nil {
		log.Println("getblob", key, err)
		w.WriteHeader(500)
		fmt.Fprintln(w, err)
		return
	}
	switch content.status {
	case ContentCached:
		if firsttime {
			nCacheHit.Add(1)
			log.Println("Cache Hit", key)
			w.Header().Set("X-Cached", "1")
		}
		defer content.r.Close()
	case ContentLarge:
		log.Println("Cache Miss (too large)", key)
		w.Header().Set("X-Cached", "2")
		defer content.r.Close()
	case ContentWaiting:
		if !firsttime {
			// why are we waiting for content a second time?
			log.Println("getblob", key, "unexpectedly waiting for content a second time")
			w.WriteHeader(500)
			fmt.Fprintln(w, "The file cannot be accessed at this time")
			return
		}
		nCacheMiss.Add(1)
		log.Println("Cache Miss", key)
		w.Header().Set("X-Cached", "0")
		// Since content is not returned for non-GET requests, don't wait
		// for it to be cached.
		if r.Method != "GET" {
			break
		}
		select {
		case <-content.done:
			log.Println("Waiting for content is done, trying again", key)
			firsttime = false
			goto retry
		case <-time.After(60 * time.Second):
			log.Println("getblob", key, "timeout")
			w.WriteHeader(504)
			fmt.Fprintln(w, "timeout")
			return
		}
	default:
		log.Println("getblob received status", content.status)
		w.WriteHeader(500)
		fmt.Fprintln(w, "received status", content.status)
		return
	}

	w.Header().Set("ETag", fmt.Sprintf(`"%d"`, bid))
	// use ServeContent to support range requests. Fall back to io.Copy if the
	// data source does not support seeks.
	if c, ok := content.r.(io.ReadSeeker); ok {
		http.ServeContent(w, r, "", time.Time{}, c)
		return
	}

	w.Header().Set("Content-Length", fmt.Sprintf("%d", content.size))

	// all the headers have been set, now do we need to copy bits?
	if r.Method != "GET" {
		return
	}
	n, err := io.Copy(w, content.r)
	if err != nil {
		log.Printf("getblob (%s,%d) %d,%s", id, bid, n, err.Error())
	}
}

// contentSource is either a ReadCloser that contains the requested data, or it is a promise of a future data stream, which is ready when the done channel is closed.
type contentSource struct {
	status ContentStatus
	r      io.ReadCloser              // valid if status is Cached or Large
	size   int64                      // valid if status is Cached, Large, or Waiting
	done   <-chan singleflight.Result // valid if status is Waiting
}

type ContentStatus int

const (
	ContentUnknown ContentStatus = iota
	ContentCached                // the content is sourced from the cache
	ContentLarge                 // the content is very big and is not cached
	ContentWaiting               // the content is being copied into the cache
)

// An errorlist is a simple goroutine safe map that expires entries
// based on time.
type errorlist struct {
	m sync.Mutex

	// since not many errors are expected, use a list instead of a map since it
	// is simpler, and ordering entries by time makes it easier to prune old
	// entries.
	errs []errorentry
}

type errorentry struct {
	key     string
	err     error
	expires time.Time
}

func (e *errorlist) add(key string, err error) {
	e.m.Lock()
	e.errs = append(e.errs, errorentry{
		key:     key,
		err:     err,
		expires: time.Now().Add(30 * time.Second),
	})
	e.m.Unlock()
}

// find scans the list for an unexpired error for the given  key. It returns
// either the most recent error or nil.
func (e *errorlist) find(key string) error {
	var result error
	now := time.Now()
	e.m.Lock()
	// scan the list backward for an entry having the key,
	// so we can stop once we hit an expired entry.
	i := len(e.errs) - 1
	for ; i >= 0; i-- {
		if e.errs[i].expires.Before(now) {
			// entries are sorted by expire times, so the rest
			// of the list has expired.
			break
		}
		if e.errs[i].key == key {
			result = e.errs[i].err
			goto out
		}
	}
	// didn't find a match for key. Remove any expired entires.
	if i >= 0 {
		e.errs = e.errs[i+1:]
	}
out:
	e.m.Unlock()
	return result
}

// findContent will look in the cache and on tape for the given blob. If
// it is not in the cache, it will load it into the cache, if doLoad is true.
// (This is to facilitate HEAD requests that shouldn't recall content).
func (s *RESTServer) findContent(key string, id string, bid items.BlobID, doLoad bool) (contentSource, error) {
	var result contentSource
	cacheContents, length, err := s.Cache.Get(key)
	if err != nil {
		return result, err
	}
	if cacheContents != nil {
		// item was cached
		result.status = ContentCached
		result.r = NewReadSeekCloser(cacheContents, length)
		result.size = length
		return result, nil
	}
	// need to source the content from tape
	if !s.useTape {
		return result, items.ErrNoStore
	}
	blobinfo, err := s.Items.BlobInfo(id, bid)
	if err != nil {
		return result, err
	}
	length = blobinfo.Size
	result.size = length
	if !doLoad {
		result.status = ContentWaiting
		return result, nil
	}
	// were there previous errors when caching this blob?
	err = s.errorledger.find(key)
	if err != nil {
		return result, err
	}
	// cache this item if it is not too large.
	// doing 1/8th of the cache size is arbitrary.
	// not sure what a good cutoff would be.
	// (remember maxsize == 0 means infinite)
	cacheMaxSize := s.Cache.MaxSize()
	if cacheMaxSize == 0 || length < cacheMaxSize/8 {
		// single flight the requests
		// lazy initialize
		if s.tapeinflight == nil {
			s.tapeinflight = &singleflight.Group{}
		}
		c := s.tapeinflight.DoChan(key, func() (interface{}, error) {
			s.copyBlobIntoCache(key, id, bid)
			return nil, nil
		})
		result.status = ContentWaiting
		result.done = c
		return result, nil
	}
	// item is too large to be cached
	// get it directly from tape
	realContents, _, err := s.Items.Blob(id, bid)
	if err != nil {
		return result, err
	}
	result.status = ContentLarge
	result.r = realContents
	return result, nil
}

// copyBlobIntoCache copies the given blob of the item id into s's blobcache
// under the given key. Closes the given channel when the item is copied or if
// there was an error. Errors are added to the errorledger.
func (s *RESTServer) copyBlobIntoCache(key, id string, bid items.BlobID) {
	starttime := time.Now()
	var keepcopy bool
	// defer this first so it is the last to run at exit.
	// because cw needs to be Closed() before the Delete().
	// And defered funcs are run LIFO.
	defer func() {
		if !keepcopy {
			s.Cache.Delete(key)
		}
		log.Println("copyblob finished", key, time.Now().Sub(starttime))
	}()
	cw, err := s.Cache.Put(key)
	if err != nil {
		// since there is a gaurd around calling copyBlobIntoCache() we
		// shouldn't be receiving ErrPutPending errors here...
		log.Printf("cache put %s: %s", key, err.Error())
		keepcopy = true // in case someone else added a copy already
		return
	}
	defer func() {
		err := cw.Close()
		if err != nil {
			// also want to also put this into the errorlog, but don't want to
			// potentially shadow any earlier errors that may have been put
			// there in this effort. So for now we just log it.
			log.Println("cache close", key, err)
			keepcopy = false
		}
	}()
	cr, length, err := s.Items.Blob(id, bid)
	if err != nil {
		log.Printf("cache items get %s: %s", key, err.Error())
		s.errorledger.add(key, err)
		return
	}
	defer cr.Close()
	// should we put a timeout on the copy?
	n, err := io.Copy(cw, cr)
	if err != nil {
		log.Printf("cache copy %s: %s", key, err.Error())
		s.errorledger.add(key, err)
		return
	}
	if n != length {
		err = fmt.Errorf("cache length mismatch: read %d, expected %d", n, length)
		log.Println(err)
		s.errorledger.add(key, err)
		return
	}
	keepcopy = true
}

// NewReadSeekCloser converts a ReadAtCloser into a ReadSeekCloser.
func NewReadSeekCloser(r store.ReadAtCloser, size int64) io.ReadSeekCloser {
	return &readseekcloser{r: r, size: size}
}

var (
	ErrInvalidPos = errors.New("Seek: cannot seek before read position")
	ErrWhence     = errors.New("Seek: invalid whence")
)

type readseekcloser struct {
	r    store.ReadAtCloser
	off  int64
	size int64
}

func (r *readseekcloser) Read(p []byte) (n int, err error) {
	n, err = r.r.ReadAt(p, r.off)
	r.off += int64(n)
	if err == io.EOF && n > 0 {
		// reading less than a full buffer is not an error for
		// an io.Reader
		err = nil
	}
	return
}

func (r *readseekcloser) Close() error {
	return r.r.Close()
}

// Seek implements the io.Seek() interface
func (r *readseekcloser) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.off + offset
	case io.SeekEnd:
		abs = r.size + offset
	default:
		return 0, ErrWhence
	}
	if abs < 0 {
		return 0, ErrInvalidPos
	}
	if abs > r.size {
		abs = r.size
	}
	r.off = abs
	return abs, nil
}

// ItemHandler handles requests to GET /item/:id
func (s *RESTServer) ItemHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	id := ps.ByName("id")
	item, err := s.Items.Item(id)
	if err != nil {
		// If Item Store Disable, return a 503
		if err == items.ErrNoStore {
			w.WriteHeader(503)
			log.Printf("GET /item/%s returns 503 - tape disabled", id)
		} else {
			w.WriteHeader(404)
		}
		fmt.Fprintln(w, err.Error())
		return
	}
	// sometimes when there are storage errors no Version list gets saved to tape.
	if len(item.Versions) > 0 {
		vid := item.Versions[len(item.Versions)-1].ID
		w.Header().Set("ETag", fmt.Sprintf(`"%d"`, vid))
	}
	writeHTMLorJSON(w, r, itemTemplate, item)
}

func minus1(a interface{}) int {
	// the template calls this with something having type BlobID, so we make a
	// have type interface{}, and type switch to get the right value
	switch v := a.(type) {
	case int:
		return v - 1
	case items.BlobID:
		return int(v) - 1
	}
	return 0
}

var (
	itemfns = template.FuncMap{
		"minus1": minus1,
	}

	itemTemplate = template.Must(template.New("items").Funcs(itemfns).Parse(`
<html><head><style>
tbody tr:nth-child(even) { background-color: #eeeeee; }
</style></head><body>
<h1>Item {{ .ID }}</h1>
<table>
	<thead><tr>
		<th>Version</th>
		<th>Date</th>
		<th>Creator</th>
		<th>Note</th>
	</tr></thead><tbody>
{{ range .Versions }}
	<tr>
		<td>{{ .ID }}</td>
		<td>{{ .SaveDate }}</td>
		<td>{{ .Creator }}</td>
		<td>{{ .Note }}</td>
	</tr>
{{ end }}
</tbody></table>
<dl>
<dt>MaxBundle</dt><dd>{{ .MaxBundle }}</dd>
</dl>
{{ $blobs := .Blobs }}
{{ $id := .ID }}
{{ with index .Versions (len .Versions | minus1) }}
	<h2>Version {{ .ID }}</h2>
	<table><thead><tr>
		<th>Bundle</th>
		<th>Blob</th>
		<th>Size</th>
		<th>Date</th>
		<th>MimeType</th>
		<th>MD5</th>
		<th>SHA256</th>
		<th>Filename</th>
	</tr></thead><tbody>
	{{ range $key, $value := .Slots }}
		<tr>
		{{ with index $blobs ($value | minus1) }}
			<td>{{ .Bundle }}</td>
			<td><a href="/item/{{ $id }}/@blob/{{ $value }}">{{ $value }}</a></td>
			<td>{{ .Size }}</td>
			<td>{{ .SaveDate }}</td>
			<td>{{ .MimeType }}</td>
			<td>{{ printf "%x" .MD5 }}</td>
			<td>{{ printf "%x" .SHA256 }}</td>
		{{ end }}
		<td><a href="/item/{{ $id }}/{{ $key }}">{{ $key }}</a></td>
		</tr>
	{{ end }}
	</tbody></table>
{{ end }}
</body></html>`))
)

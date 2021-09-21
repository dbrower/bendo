package blobcache

import (
	"fmt"
	"testing"
	"time"

	"github.com/ndlib/bendo/store"
)

func TestEvictionTB(t *testing.T) {
	cache := NewTime(store.NewMemory(), time.Second)
	defer cache.Stop()
	// add item, see if it goes away
	w, err := cache.Put("hello")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("hello world"))
	w.Close()

	time.Sleep(1200 * time.Millisecond)
	r, _, err := cache.Get("hello")
	if r != nil {
		t.Error("Key not evicted")
	}
}

func TestExpireListTB(t *testing.T) {
	cache := NewTime(store.NewMemory(), time.Second)
	defer cache.Stop()
	// add items
	for i := 0; i < 100; i++ {
		w, _ := cache.Put(fmt.Sprintf("hello-%d", i))
		w.Write([]byte("hello world"))
		w.Close()
	}

	// sleep less than expire time and then touch half of the test values
	time.Sleep(500 * time.Millisecond)
	for i := 0; i < 100; i += 2 {
		r, _, _ := cache.Get(fmt.Sprintf("hello-%d", i))
		if r == nil {
			t.Error("key", i, "unexpectably evicted")
			continue
		}
		r.Close()
	}

	// sleep a bit and see if the untouched items are gone
	time.Sleep(600 * time.Millisecond)
	for i := 0; i < 100; i++ {
		r, _, _ := cache.Get(fmt.Sprintf("hello-%d", i))
		if r == nil {
			if i%2 == 0 {
				t.Error("Even key unexpectably evicted", i)
			}
			continue
		}
		if i%2 != 0 {
			t.Error("Odd key not evicted", i)
		}
		r.Close()
	}
}

func TestItemReading(t *testing.T) {
	mem := store.NewMemory()
	cache := NewTime(mem, time.Second)
	cache.Stop() // turn off background process
	// add item
	w, _ := cache.Put("sample-item")
	w.Write([]byte("hello world"))
	w.Close()

	// remember this cache entry
	entry := cache.items["sample-item"]

	// write item manifest
	cache.writeIndexFile()

	// sleep a tad
	time.Sleep(50 * time.Millisecond)

	// now start a new cache and see if the item has the same expire time.
	// this tests whether the manifest file was read correctly
	cache2 := NewTime(mem, time.Second)
	cache2.Stop()
	cache2.Scan()

	entry2 := cache2.items["sample-item"]
	if !entry.Expires.Equal(entry2.Expires) {
		t.Error("Expected expiry", entry.Expires, "Got", entry2.Expires)
	}
}

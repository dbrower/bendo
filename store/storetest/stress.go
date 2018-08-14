// storetest provides functions for facilitating the testing of anything
// implementing the Store interface.
package storetest

import (
	"bytes"
	"crypto/md5"
	"io"
	"math"
	"math/rand"
	"sync"
	"testing"

	"github.com/ndlib/bendo/store"
)

type blob struct {
	key  string
	hash []byte
	size int64
}

// Stress will spawn a given number of goroutines to simultainously
// try reading and writing to the given store. It is a good test to run
// with the -race flag to try to race conditions.
//
// Generate a list of sizes, until their sum is >= totalsize.
// For each size, upload a random blob of that size, and then download it
// and compare it for correctness.
//
// randomly delete the blob or try downloading it again.
// At some point every blob will be deleted. End the test.
//
// Note, this does not test the list or list prefix functions.
func Stress(t *testing.T, s store.Store, totalsize int64) {
	// the pipeline is
	//       size maker
	// sizes ----> uploader pool
	// dwnld ----> downloader pool (possible repeat)
	//       ----> delete
	if totalsize == 0 {
		totalsize = 1000 * 1000 * 1000 // 1GB
	}
	sizes := make(chan int64)
	dwnld := make(chan blob, 1000)
	done := make(chan struct{})
	var uppool, downpool sync.WaitGroup

	for i := 0; i < 5; i++ {
		uppool.Add(1)
		go func() {
			uploader(t, s, sizes, dwnld)
			uppool.Done()
		}()
	}

	for i := 0; i < 10; i++ {
		downpool.Add(1)
		go func() {
			downloader(t, s, dwnld, done)
			downpool.Done()
		}()
	}

	generatesizes(sizes, totalsize)
	close(sizes)
	uppool.Wait()
	close(done)
	downpool.Wait()
}

// randomReader is provides an interface to n bytes of random data.
// The length may be much longer than len(data).
type randomReader struct {
	n    int64
	data []byte
}

func (r *randomReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, io.EOF
	}
	total := 0
	data := r.data
	for len(p) > 0 && r.n > 0 {
		if r.n < int64(len(data)) {
			data = data[:int(r.n)]
		}
		n := copy(p, data)
		p = p[n:]
		r.n -= int64(n)
		total += n
	}
	return total, nil
}

func uploader(t *testing.T, s store.Store, in <-chan int64, out chan<- blob) {
	h := md5.New()
	const L = 64 * 1024 // 64k
	buffer := make([]byte, L)

	for size := range in {
		h.Reset()
		buffer = buffer[:L] // make sure buffer is full sized
		rand.Read(buffer)

		// get the key name by picking ascii characters out of buffer
		// first byte gives offset, then get length, then start looking
		j := buffer[0]
		klen := int(buffer[j]) & 0x3f // ensure length < 64
		klen += 1                     // and > 0
		key := make([]byte, 0, klen)
		// we don't check for j being inside buffer...
		// statistically we shouldn't run off the end :)
		for ; len(key) < klen; j++ {
			if buffer[j] >= 'a' && buffer[j] <= 'z' {
				key = append(key, buffer[j])
			}
		}
		keystr := string(key)
	retry:
		w, err := s.Create(keystr)
		if err == store.ErrKeyExists {
			keystr += "a"
			goto retry
		} else if err != nil {
			t.Error(err)
			continue
		}
		mw := io.MultiWriter(h, w)
		// upload size bytes
		n, err := io.Copy(mw, &randomReader{data: buffer, n: size})
		if n != size {
			t.Error("expected", size, "only read", n)
		}
		if err != nil {
			t.Error(err)
		}
		err = w.Close()
		if err != nil {
			t.Error(keystr, size, err)
			continue
		}
		out <- blob{key: keystr, hash: h.Sum(nil)[:], size: size}
	}
}

func downloader(t *testing.T, s store.Store, in chan blob, done chan struct{}) {
	h := md5.New()
	for {
		var blob blob
		select {
		case <-done:
			return
		case blob = <-in:
		}
		rac, size, err := s.Open(blob.key)
		if err != nil {
			t.Error(err)
			continue
		}
		if size != blob.size {
			t.Error("Expected", blob.size, "Get() returned", size)
		}
		h.Reset()
		n, err := io.Copy(h, store.NewReader(rac))
		if err != nil {
			t.Error(err)
		}
		if n != size {
			t.Error("Expected", size, "but read", n)
		}
		err = rac.Close()
		if err != nil {
			t.Error(err)
		}
		if !bytes.Equal(blob.hash, h.Sum(nil)) {
			t.Errorf("hashes unequal. %#v. Received %x", blob, h.Sum(nil))
			// note that the item is left in the store...
			continue
		}

		// figure out what to do next
		x := rand.Float32()
		switch {
		case x < 0.5:
			err := s.Delete(blob.key)
			if err != nil {
				t.Error(err)
			}
		default:
			// reinsert once
			in <- blob
		}
	}
}

func generatesizes(out chan<- int64, totalsize int64) {
	// We want a wide range of sizes, so generate the exponent of the size
	// uniformly at random.
	//  choose number x ~ uniform(0, 20)
	//  let size be exp(x)
	for totalsize > 0 {
		x := 20 * rand.Float64()
		size := int64(math.Trunc(math.Exp(x)))
		out <- size
		totalsize -= size
	}
}

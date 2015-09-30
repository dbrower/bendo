package items

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/ndlib/bendo/store"
)

/*
BundleWriter helps with saving blobs into bundles, and with repackaging blobs
when doing deletions. It keeps a reference to its source item, and will use
that to save the item-info.json file when needed.

It is not goroutine safe. Make sure to call Close when finished.
*/
type BundleWriter struct {
	store store.Store
	item  *Item
	zw    *Zipwriter // target bundle file. nil if nothing is open.
	size  int64      // amount written to current bundle
	n     int        // 1 + current bundle id
}

// NewBundler starts a new bundle writer for the given item. More than one bundle
// file may be written. The advancement to a new bundle file happens either when
// the current one grows larger than IdealBundleSize, or when Next() is called.
func NewBundler(s store.Store, item *Item) *BundleWriter {
	bw := &BundleWriter{
		store: s,
		item:  item,
		n:     item.MaxBundle + 1,
	}
	// force us to open a blob file.
	bw.Next() // ignore error. next call to WriteBlob will retrigger it
	return bw
}

// CurrentBundle returns the id of the bundle being written to.
func (bw *BundleWriter) CurrentBundle() int {
	if bw.zw == nil {
		return bw.n
	}
	return bw.n - 1
}

// Next closes the current bundle, if any, and starts a new bundle file.
func (bw *BundleWriter) Next() error {
	var err error
	err = bw.Close()
	if err != nil {
		return err
	}
	bw.zw, err = OpenZipWriter(bw.store, bw.item.ID, bw.n)
	if err != nil {
		return err
	}
	bw.n++
	bw.size = 0
	return nil
}

// Close writes out any final metadata and closes the current bundle.
func (bw *BundleWriter) Close() error {
	if bw.zw == nil {
		return nil
	}
	// write out the item data
	w, err := bw.zw.MakeStream("item-info.json")
	if err == nil {
		err = writeItemInfo(w, bw.item)
	}
	bw.zw.Close()
	bw.zw = nil
	return err
}

const (
	// MB is the number of bytes in one megabyte (we use base 10)
	MB = 1000000

	// IdealBundleSize is a cutoff, and new bundle files will be started
	// once the current one grows past this. (only checked when starting
	// as new blob.)
	IdealBundleSize = 500 * MB
)

// WriteBlob writes the given blob into the bundle.
func (bw *BundleWriter) WriteBlob(blob *Blob, r io.Reader) error {
	if bw.size >= IdealBundleSize || bw.zw == nil {
		if err := bw.Next(); err != nil {
			return err
		}
	}
	w, err := bw.zw.MakeStream(fmt.Sprintf("blob/%d", blob.ID))
	if err != nil {
		return err
	}
	size, err := io.Copy(w, r)
	bw.size += size
	if err != nil {
		return err
	}
	// Don't update DateSaved timestamp, since the blob may be a copy
	// because of a purge
	blob.Bundle = bw.n - 1
	if blob.Size == 0 {
		blob.Size = size
	} else if blob.Size != size {
		return fmt.Errorf("commit (%s blob %d), copied %d bytes, expected %d",
			bw.item.ID,
			blob.ID,
			size,
			blob.Size)
	}
	checksums := bw.zw.Checksum()
	err = testhash(checksums.MD5, &blob.MD5, bw.item.ID)
	if err == nil {
		err = testhash(checksums.SHA256, &blob.SHA256, bw.item.ID)
	}
	return err
}

func testhash(h []byte, target *[]byte, name string) error {
	if *target == nil {
		*target = h
	} else if bytes.Compare(*target, h) != 0 {
		return fmt.Errorf("commit (%s), got %s, expected %s",
			name,
			hex.EncodeToString(h),
			hex.EncodeToString(*target))
	}
	return nil
}

// CopyBundleExcept copies all the blobs in the bundle src, except for those in
// the list, into the current place in the bundle writer.
func (bw *BundleWriter) CopyBundleExcept(src int, except []BlobID) error {
	r, err := OpenBundle(bw.store, sugar(bw.item.ID, src))
	if err != nil {
		return err
	}
	defer r.Close()
	var badnames = make([]string, 1+len(except))
	badnames[0] = "item-info.json"
	for i, id := range except {
		badnames[i+1] = fmt.Sprintf("blob/%d", id)
	}
	for _, fname := range r.Files() {
		if contains(badnames, fname) {
			continue
		}
		var rc io.ReadCloser
		rc, err = r.Open(fname)
		if err != nil {
			return err
		}
		// TODO(dbrower): check for errors
		blob := bw.item.blobByID(extractBlobID(fname))
		err = bw.WriteBlob(blob, rc)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func contains(lst []string, s string) bool {
	for i := range lst {
		if lst[i] == s {
			return true
		}
	}
	return false
}

// from "blob/xxx" return xxx as a BlobID
func extractBlobID(s string) BlobID {
	sa := strings.SplitN(s, "/", 2)
	if len(sa) != 2 || sa[0] != "blob" {
		return BlobID(0)
	}
	id, err := strconv.ParseInt(sa[1], 10, 0)
	if err != nil {
		id = 0
	}
	return BlobID(id)
}

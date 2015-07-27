/*
Fragment manages the fragment cache used to upload files to the server.
The fragment cache lets files be uploaded in pieces, and then be copied
to tape as a single unit. Files are intended to be uploaded as consecutive
pieces, of arbitrary size. If a fragment upload does not complete or has
and error, that fragment is deleted, and the upload can try again.
*/
package fragment

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/ndlib/bendo/store"
)

// Store wraps a store.Store and provides a fragment cache. This allows files
// to be uploaded in pieces, "fragments", and then read back as a single
// unit.
type Store struct {
	s     store.Store
	m     sync.RWMutex // protects files
	files map[string]*File
}

const (
	// we store two kinds of information in the store: file metadata and
	// file fragments. They are distinguished by the prefix of their keys:
	// metadata keys start with "md" and file fragments start with "f".
	//
	// we keep the metadata info in the store to allow resumption after
	// server restarts.
	fileKeyPrefix     = "md"
	fragmentKeyPrefix = "f"
)

type File struct {
	s    store.Store
	ID   string // does not include fileKeyPrefix
	Size int64
	// Children ids, in the order to read them.
	// They already include the fragmentKeyPrefix
	Children []*fragment
}

type fragment struct {
	ID   string // includes fragmentKeyPrefix
	Size int64
}

// Create a new fragment store wrapping a store.Store. It will try to load its
// metadata from the store before returning.
func New(s store.Store) (*Store, error) {
	news := &Store{
		s:     s,
		files: make(map[string]*File),
	}
	err := news.load()
	return news, err
}

// initialize our in memory data from the store.
func (s *Store) load() error {
	metadata, err := s.s.ListPrefix(fileKeyPrefix)
	if err != nil {
		return err
	}
	s.m.Lock()
	defer s.m.Unlock()
	for _, key := range metadata {
		r, _, err := s.s.Open(key)
		if err != nil {
			return err
		}
		f, err := load(r)
		r.Close()
		if err != nil {
			// TODO(dbrower): this is probably too strict. We should
			// probably just skip this file
			return err
		}
		f.s = s.s
		// we could deconstruct the ID from the key, but
		// it is easier to just unmarshal it from the json
		s.files[f.ID] = f
	}
	return nil
}

// Create a new file with the given name, and return a pointer to it.
// the file is not persisted until its first fragment has been written.
// It is an error to create a file which already exists.
func (s *Store) New(id string) *File {
	s.m.Lock()
	defer s.m.Unlock()
	if _, ok := s.files[id]; ok {
		panic("File already exists")
	}
	newfile := &File{ID: id, s: s.s}
	s.files[id] = newfile
	return newfile
}

// Lookup a file. Returns nil if none exists with that id. Files returned
// from here are not goroutine safe.
func (s *Store) Lookup(id string) *File {
	s.m.RLock()
	defer s.m.RUnlock()
	return s.files[id]
}

// Delete a file
func (s *Store) Delete(id string) {
	s.m.Lock()
	f := s.files[id]
	delete(s.files, id)
	s.m.Unlock()

	if f == nil {
		return
	}

	// don't need the lock for the following
	s.s.Delete(fileKeyPrefix + f.ID)
	for _, child := range f.Children {
		s.s.Delete(child.ID)
	}
}

// Open a file for writing. Any writes are appended to the end.
func (f *File) Append() (io.WriteCloser, error) {
	fragkey := fmt.Sprintf("%s%s%04d",
		fragmentKeyPrefix,
		f.ID,
		len(f.Children))
	w, err := f.s.Create(fragkey)
	if err != nil {
		return nil, err
	}
	frag := &fragment{ID: fragkey}
	return &fragwriter{frag: frag, file: f, w: w}, nil
}

type fragwriter struct {
	frag *fragment
	file *File
	w    io.WriteCloser
}

func (fw *fragwriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.frag.Size += int64(n)
	return n, err
}

func (fw *fragwriter) Close() error {
	err := fw.w.Close()
	if err == nil {
		fw.file.Children = append(fw.file.Children, fw.frag)
		fw.file.Size += fw.frag.Size
		err = fw.file.save()
	}
	return err
}

// open a file for reading
func (f *File) Open() io.ReadCloser {
	return &fragreader{
		s:     f.s,
		frags: f.Children[:],
	}
}

type fragreader struct {
	s     store.Store
	frags []*fragment
	r     store.ReadAtCloser
	off   int64
}

func (fr *fragreader) Read(p []byte) (int, error) {
	var err error
	for len(fr.frags) > 0 {
		if fr.r == nil {
			fr.r, _, err = fr.s.Open(fr.frags[0].ID)
			if err != nil {
				return 0, err
			}
			fr.off = 0
			fr.frags = fr.frags[1:]
		}
		n, err := fr.r.ReadAt(p, fr.off)
		fr.off += int64(n)
		if err == io.EOF {
			fr.r.Close()
			fr.r = nil
			err = nil
		}
		if n > 0 || err != nil {
			return n, err
		}
	}
	return 0, io.EOF
}

func (fr *fragreader) Close() error {
	if fr.r != nil {
		return fr.r.Close()
	}
	return nil
}

func (f *File) Info()     {}
func (f *File) Rollback() {}

func load(r io.ReaderAt) (*File, error) {
	dec := json.NewDecoder(&reader{r: r})
	f := new(File)
	err := dec.Decode(f)
	if err != nil {
		f = nil
	}
	return f, err
}

func (f *File) save() error {
	key := fileKeyPrefix + f.ID
	err := f.s.Delete(key)
	if err != nil {
		return err
	}
	w, err := f.s.Create(key)
	if err != nil {
		return err
	}
	defer w.Close()
	enc := json.NewEncoder(w)
	return enc.Encode(f)
}

// defer opening the given key until Read is called.
// close the stream when EOF is reached

// Turn a ReaderAt into a io.Reader
type reader struct {
	r   io.ReaderAt
	off int64
}

func (r *reader) Read(p []byte) (n int, err error) {
	n, err = r.r.ReadAt(p, r.off)
	r.off += int64(n)
	if err == io.EOF && n > 0 {
		// reading less than a full buffer is not an error for
		// an io.Reader
		err = nil
	}
	return
}

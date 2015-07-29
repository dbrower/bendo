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
	// There are two kinds of information in the store: file metadata and
	// file fragments. They are distinguished by the prefix of their keys:
	// metadata keys start with "md" and file fragments start with "f".
	//
	// The metadata info is in the store to allow reloading after
	// server restarts.
	fileKeyPrefix     = "md"
	fragmentKeyPrefix = "f"
)

type File struct {
	// what kind of locking is needed on a File?
	s        store.Store
	ID       string      // does not include fileKeyPrefix
	Size     int64       // sum of all the children sizes
	N        int         // the id number to use for the next fragment
	Children []*Fragment // Children ids, in the order to read them.
}

type Fragment struct {
	ID   string // includes fragmentKeyPrefix
	Size int64
}

// Create a new fragment store wrapping a store.Store. The metadata will be
// loaded from the store before returning.
func New(s store.Store) *Store {
	return &Store{
		s:     s,
		files: make(map[string]*File),
	}
}

// Initialize our in-memory data from the store. This must be called before
// using the structure.
func (s *Store) Load() error {
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

// Return a list of all the stored file names.
func (s *Store) List() []string {
	s.m.RLock()
	defer s.m.RUnlock()
	result := make([]string, 0, len(s.files))
	for k := range s.files {
		result = append(result, k)
	}
	return result
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

// Lookup a file. Returns nil if none exists with that id.
// Returned pointers are not safe to be accessed by more than one goroutine.
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

// Open a file for writing. The writes are appended to the end.
func (f *File) Append() (io.WriteCloser, error) {
	fragkey := fmt.Sprintf("%s%s%04d",
		fragmentKeyPrefix,
		f.ID,
		f.N)
	f.N++
	w, err := f.s.Create(fragkey)
	if err != nil {
		return nil, err
	}
	frag := &Fragment{ID: fragkey}
	return &fragwriter{frag: frag, file: f, w: w}, nil
}

type fragwriter struct {
	frag *Fragment
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

// Open a file for reading from the beginning.
func (f *File) Open() io.ReadCloser {
	return &fragreader{
		s:         f.s,
		fragments: f.Children[:],
	}
}

// fragreader provides an io.Reader which will span a list of fragments.
// Each fragment is opened and closed in turn, so there is at most one
// file descriptor open at any time.
type fragreader struct {
	s         store.Store
	fragments []*Fragment
	r         store.ReadAtCloser // nil if no reader is open
	off       int64              // offset into r to read from next
}

func (fr *fragreader) Read(p []byte) (int, error) {
	for len(fr.fragments) > 0 {
		var err error
		if fr.r == nil {
			// need to open a new reader
			fr.r, _, err = fr.s.Open(fr.fragments[0].ID)
			if err != nil {
				return 0, err
			}
			fr.off = 0
			fr.fragments = fr.fragments[1:]
		}
		n, err := fr.r.ReadAt(p, fr.off)
		fr.off += int64(n)
		if err == io.EOF {
			// need to check rest of list before sending EOF
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

// Remove the last fragment from this file.
// Use RemoveFragment to remove a specific fragment.
func (f *File) Rollback() error {
	return f.RemoveFragment(len(f.Children) - 1)
}

// Remove the given fragment from a file. The first fragment is
// 0, the next is 1, etc. Use Rollback() to remove the last fragment.
func (f *File) RemoveFragment(n int) error {
	if n < 0 || n >= len(f.Children) {
		return nil
	}
	frag := f.Children[n]
	err := f.s.Delete(frag.ID)
	if err != nil {
		return err
	}
	f.Children = append(f.Children[:n], f.Children[n+1:]...)
	f.Size -= frag.Size
	return f.save()
}

func load(r io.ReaderAt) (*File, error) {
	dec := json.NewDecoder(store.NewReader(r))
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

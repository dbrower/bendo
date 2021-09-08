package bclientapi

import (
	"bytes"
	"log"
	"net/http/httptest"
	"testing"

	"github.com/ndlib/bendo/blobcache"
	"github.com/ndlib/bendo/fragment"
	"github.com/ndlib/bendo/items"
	"github.com/ndlib/bendo/server"
	"github.com/ndlib/bendo/store"
	"github.com/ndlib/bendo/transaction"
)

func TestChunkAndUpload(t *testing.T) {
	data := "0123456789abcdefghijklmnopqrstuvwxyz"
	r := bytes.NewReader([]byte(data))
	md5 := []byte{0xe9, 0xb1, 0x71, 0x3d, 0xb6, 0x20, 0xf1, 0xe3, 0xa1, 0x4b, 0x68, 0x12, 0xde, 0x52, 0x3f, 0x4b}

	for i := 0; i < 8; i++ {
		log.Println("i=", i)
		eserver, remote := NewLocalBendoServer()
		// set this to give an error on the ith API call.
		eserver.Reset([]Play{Play{When: i, Status: 412}})

		conn := &Connection{
			HostURL:   remote.URL,
			ChunkSize: 10, // bytes
		}

		err := conn.upload("qwerty-12345", r, FileInfo{MD5: md5, Mimetype: "application/other"})
		if err != nil {
			t.Error(err)
		}
	}
}

func TestUpload(t *testing.T) {
	_, remote := NewLocalBendoServer()

	c := &Connection{
		HostURL:   remote.URL,
		ChunkSize: 10, // bytes
	}
	data := "0123456789abcdefghijklmnopqrstuvwxyz"
	r := bytes.NewReader([]byte(data))

	err := c.Upload("abcd", r, FileInfo{})
	t.Log(err)
}

func NewLocalBendoServer() (*ErrorServer, *httptest.Server) {
	db, _ := server.NewQlCache("mem--server")
	bendo := &server.RESTServer{
		Validator:      server.NobodyValidator{},
		Items:          items.NewWithCache(store.NewMemory(), items.NewMemoryCache()),
		TxStore:        transaction.New(store.NewMemory()),
		FileStore:      fragment.New(store.NewMemory()),
		Cache:          blobcache.NewLRU(store.NewMemory(), 400),
		FixityDatabase: db,
		//useTape:        true,
	}

	e := &ErrorServer{h: bendo.Handler()}
	return e, httptest.NewServer(e)
}

package bclientapi

import (
	"errors"
	"fmt"
	"path"
	"time"

	"github.com/ndlib/bendo/transaction"
)

// A Connection represents a connection with a Bendo Service.
// It can be shared between multiple goroutines.
type Connection struct {
	// The bendo server this connection is to
	HostURL string

	Fileroot  string
	Item      string
	ChunkSize int
	Wait      bool
	Token     string
}

// serve file requests from the server for  a get
// If the file Get fails, close the channel and exit

func (c *Connection) GetFiles(fileQueue chan string, pathPrefix string) error {

	for filename := range fileQueue {
		err := c.downLoad(filename, pathPrefix)

		if err != nil {
			fmt.Printf("Error: GetFile return %s\n", err.Error())
			return err
		}
	}

	return nil
}

// upload the give file to the bendo server

func (c *Connection) UploadFile(filename string, uploadMd5 []byte, mimetype string) error {
	_, err := c.chunkAndUpload(filename, uploadMd5, mimetype)

	// If an error occurred, report it, and return

	if err != nil {
		// add api call to delete fileid uploads
		fmt.Printf("Error: unable to upload file %s for item %s, %s\n", filename, c.Item, err)
		return err
	}

	return nil

}

var (
	ErrTransaction = errors.New("error processing transaction")
	ErrTimeout     = errors.New("timeout processing transaction")
)

// WaitForCommitFinish waits for the given transaction to finish.
// It will return an error if the transaction had an error.
// It will poll the server for up to 12 hours, and then return
// a timeout error.
func (c *Connection) WaitForCommitFinish(txpath string) error {
	txid := path.Base(txpath)

	fmt.Printf("Waiting on transaction %s:", txid)

	// loop for at most 12 hours
	const delay = 5 * time.Second
	for i := 0; i < int(12*time.Hour/delay); i++ {
		var status int64

		fmt.Printf(".")
		time.Sleep(delay)

		v, err := c.getTransactionStatus(txid)
		if err == nil {
			status, err = v.GetInt64("Status")
		}
		if err != nil {
			return err
		}

		switch transaction.Status(status) {
		case transaction.StatusFinished:
			return nil
		case transaction.StatusError:
			fmt.Println("Error")
			errlist, _ := v.GetStringArray("Err")
			for _, e := range errlist {
				fmt.Println(e)
			}
			return ErrTransaction
		}
	}
	return ErrTimeout
}

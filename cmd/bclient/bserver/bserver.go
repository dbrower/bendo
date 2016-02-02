package bserver

import (
	"encoding/json"
	"fmt"
	"github.com/ndlib/bendo/cmd/bclient/fileutil"
	"sync"
)

var (
	fileIDMutex     sync.Mutex
	fileIDList      []fileIDStruct
)

type fileIDStruct struct {
	fileid string
	slot   string
	item   string
}

// common attributes

type itemAttributes struct {
	fileroot    string
	item        string
	bendoServer string
}

func addFileToTransactionList(filename string, fileID string, item string) {

	fileIDMutex.Lock()

	thisFileID := new(fileIDStruct)
	thisFileID.fileid = fileID
	thisFileID.slot = filename
	thisFileID.item = item

	fileIDList = append(fileIDList, *thisFileID)

	fileIDMutex.Unlock()
}

func New(server string, item string, fileroot string) *itemAttributes {
	fileutil.IfVerbose("github.com/ndlib/bendo/bclient/bserver.Init() called")

	thisItem := new(itemAttributes)
	thisItem.bendoServer = server
	thisItem.item = item
	thisItem.fileroot = fileroot

	return thisItem
}


// serve the file queue. This is called from main as 1 or more goroutines
// If the file Upload fails, close the channel and exit

func (ia *itemAttributes) SendFiles(fileQueue chan string, ld *fileutil.ListData) {

	for filename := range fileQueue {
		err := ia.uploadFile(filename, ld.ShowUploadFileMd5(filename))

		if err != nil {
			close(fileQueue)
		}
	}
}

func (ia *itemAttributes) uploadFile(filename string, uploadMd5 []byte) error {

	// upload chunks initial buffer size is 1MB

	fileID, uploadErr := ia.chunkAndUpload(filename, uploadMd5, 1048576)

	// If an error occurred, report it, and return

	if uploadErr != nil {
		// add api call to delete fileid uploads
		fmt.Printf("Error: unable to upload file %s for item %s, %s\n", filename, ia.item, uploadErr.Error())
		return uploadErr
	}

	addFileToTransactionList(filename, fileID, ia.item)

	return nil

}

func (ia *itemAttributes) SendTransactionRequest() error {

	cmdlist := [][]string{}

	for _, fid := range fileIDList {
		cmdlist = append(cmdlist, []string{"add", fid.fileid})
		cmdlist = append(cmdlist, []string{"slot", fid.slot, fid.fileid})
	}

	buf, _ := json.Marshal(cmdlist)

	transErr := ia.createFileTransAction(buf)

	//if transErr != nil {
	//       fmt.Println( transErr.Error())
	//      fmt.Printf( "Error: unable to upload file %s for item %s, %s\n", filename, ia.item, transErr.Error())
	//}

	return transErr
}

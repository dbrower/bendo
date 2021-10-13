package server

import (
	"encoding/json"
	"expvar"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"time"

	raven "github.com/getsentry/raven-go"
	"github.com/julienschmidt/httprouter"

	"github.com/ndlib/bendo/transaction"
)

// ListTxHandler handles requests to GET /transaction
func (s *RESTServer) ListTxHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	writeHTMLorJSON(w, r, listTxTemplate, s.TxStore.List())
}

var (
	listTxTemplate = template.Must(template.New("listtx").Parse(`<html>
<h1>Transactions</h1>
<ul>
{{ range . }}
	<li><a href="/transaction/{{ . }}">{{ . }}</a></li>
{{ else }}
	<li>No Transactions</li>
{{ end }}
</ul>
</html>`))
)

// TxInfoHandler handles requests to GET /transaction/:tid
func (s *RESTServer) TxInfoHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	id := ps.ByName("tid")
	tx := s.TxStore.Lookup(id)
	if tx == nil {
		w.WriteHeader(404)
		fmt.Fprintln(w, "cannot find transaction")
		return
	}
	tx.M.RLock()
	defer tx.M.RUnlock()
	writeHTMLorJSON(w, r, txInfoTemplate, tx)
}

var (
	txInfoTemplate = template.Must(template.New("txinfo").Parse(`<html>
	<h1>Transaction Info</h1>
	{{ $txid := .ID }}
	<dl>
	<dt>ID</dt><dd>{{ .ID }}</dd>
	<dt>For Item</dt><dd><a href="/item/{{ .ItemID }}">{{ .ItemID }}</a></dd>
	<dt>Status</dt><dd>{{ .Status }}</dd>
	<dt>Started</dt><dd>{{ .Started }}</dd>
	<dt>Modified</dt><dd>{{ .Modified }}</dd>
	<dt>Errors</dt><dd>{{ range .Err }}{{ . }}<br/>{{ end }}</dd>
	<dt>Commands</dt><dd>{{ range .Commands }}
		{{ if index . 0 | eq "add" }}
			{{ $fname := index . 1 }}
			[add <a href="/upload/{{ $fname }}">{{ $fname }}</a>]
		{{else}}{{ . }}
		{{ end }}
	<br/>{{ end }}</dd>
	</dl>
	<a href="/transaction">Back</a>
	</html>`))
)

// NewTxHandler handles requests to POST /item/:id/transaction
func (s *RESTServer) NewTxHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	id := ps.ByName("id")

	tx, err := s.TxStore.Create(id)
	if err != nil {
		// the err is probably that there is already a transaction open
		// on the item
		w.WriteHeader(409)
		fmt.Fprintln(w, err.Error())
		return
	}
	w.Header().Set("Location", "/transaction/"+tx.ID)
	tx.Creator = ps.ByName("username")
	// TODO(dbrower): use a limit reader to 1MB(?) for this
	var cmds [][]string
	err = json.NewDecoder(r.Body).Decode(&cmds)
	if err != nil {
		tx.SetStatus(transaction.StatusError)
		w.WriteHeader(400)
		fmt.Fprintln(w, err.Error())
		return
	}
	err = tx.AddCommandList(cmds)
	if err != nil {
		tx.SetStatus(transaction.StatusError)
		w.WriteHeader(400)
		fmt.Fprintln(w, err.Error())
		return
	}
	tx.SetStatus(transaction.StatusWaiting)
	s.txqueue <- tx.ID
	w.WriteHeader(202)
}

// transactionWorker pulls transactions off of the channel and then
// processes them. It is intended for many of these to run in parallel.
// Close s.txcancel for all workers to gracefully exit.
func (s *RESTServer) transactionWorker(queue <-chan string) {
	defer s.txwg.Done()
	for {
		var txid string
		// wait for a transaction to be queued
		select {
		case txid = <-queue:
		case <-s.txcancel:
			return
		}
		tx := s.TxStore.Lookup(txid)
		if tx == nil {
			// tx is missing...must have been deleted
			continue
		}
		switch tx.Status {
		case transaction.StatusOpen,
			transaction.StatusFinished,
			transaction.StatusError:
			// ignore and get next transaction
			continue
		}
		log.Printf("Starting transaction %s on %s (%s)",
			tx.ID,
			tx.ItemID,
			tx.Status.String())
		start := time.Now()
		switch tx.Status {
		default:
			log.Printf("Unknown status %s", tx.Status.String())
		case transaction.StatusWaiting:
			tx.SetStatus(transaction.StatusChecking)
			fallthrough
		case transaction.StatusChecking:
			tx.VerifyFiles(s.FileStore)
			if len(tx.Err) > 0 {
				tx.SetStatus(transaction.StatusError)
				goto out
			}
			tx.SetStatus(transaction.StatusIngest)
			fallthrough
		case transaction.StatusIngest:
			// make sure the tape is available. Keep looping until it is.
			for !s.useTape {
				log.Printf("Transaction %s waiting for tape availability", tx.ID)
				// wait for tape use to be enabled. for now we poll it every minute.
				select {
				case <-s.txcancel:
					return
				case <-time.After(1 * time.Minute): // this time is arbitrary
				}
			}
			tx.Commit(*s.Items, s.FileStore, s.Cache)
			s.IndexItem(tx.ItemID)
		}
	out:
		duration := time.Now().Sub(start)
		log.Printf("Finish transaction %s on %s (%s)", tx.ID, tx.ItemID, duration.String())

		xTransactionTime.Add(duration.Seconds())
		xTransactionCount.Add(1)
	}

}

var (
	xTransactionCount = expvar.NewInt("tx.count")
	xTransactionTime  = expvar.NewFloat("tx.seconds")
)

// TxCleaner will loop forever removing old transactions and old orphened
// uploaded files. That means any finished transactions which are older than a
// few days. Both the transaction and any uploaded files referenced by the
// transaction are deleted. This function will never return.
func (s *RESTServer) TxCleaner() {
	for {
		err := s.transactionCleaner()
		if err == nil {
			err = s.fileCleaner()
		}
		if err != nil {
			log.Println("TxCleaner:", err)
			raven.CaptureError(err, nil)
		}
		// wait for a while before beginning again
		time.Sleep(12 * time.Hour) // duration is arbitrary
	}
}

// transactionCleaner will go through all finished transactions (i.e. in either
// the error or successful states) which are old, and delete them and any
// uploaded files they reference. Successful transactions older than four days are
// removed, and failed transactions older than a week are removed.
func (s *RESTServer) transactionCleaner() error {
	// these time limits are completely arbitrary
	cutoffSuccess := time.Now().Add(-4 * 24 * time.Hour)
	cutoffError := time.Now().Add(-7 * 24 * time.Hour)
	for _, txid := range s.TxStore.List() {
		tx := s.TxStore.Lookup(txid)
		if tx == nil {
			continue
		}
		switch tx.Status {
		default:
			continue
		case transaction.StatusFinished:
			if tx.Modified.After(cutoffSuccess) {
				continue
			}
		case transaction.StatusError:
			if tx.Modified.After(cutoffError) {
				continue
			}
		}
		log.Printf("TxCleaner: removing transaction %s\n", txid)
		// delete every file referenced by the transaction
		for _, fid := range tx.ReferencedFiles() {
			err := s.FileStore.Delete(fid)
			if err != nil {
				return err
			}
		}
		// and delete the transaction itself
		err := s.TxStore.Delete(txid)
		if err != nil {
			return err
		}
	}
	return nil
}

// fileCleaner will remove any files in the upload cache directory older than
// two weeks. (This time is completely arbitrary).
func (s *RESTServer) fileCleaner() error {
	cutoff := time.Now().Add(-14 * 24 * time.Hour)
	for _, fid := range s.FileStore.List() {
		f := s.FileStore.Lookup(fid)
		if f == nil {
			continue
		}
		stat := f.Stat()
		if stat.Modified.After(cutoff) {
			continue
		}
		log.Printf("TxCleaner: removing file %s\n", fid)
		err := s.FileStore.Delete(fid)
		if err != nil {
			return err
		}
	}
	return nil
}

// CancelTxHandler handles requests to POST /transaction/:tid/cancel
func (s *RESTServer) CancelTxHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	tid := ps.ByName("tid")
	tx := s.TxStore.Lookup(tid)
	if tx == nil {
		w.WriteHeader(404)
		fmt.Fprintln(w, "cannot find transaction")
		return
	}
	if !(tx.Status == transaction.StatusFinished ||
		tx.Status == transaction.StatusError) {
		w.WriteHeader(400)
		fmt.Fprintf(w, "cannot delete pending transaction")
	}
	err := s.TxStore.Delete(tid)
	fmt.Fprintf(w, err.Error())
}

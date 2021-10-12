package server

import (
	"context"
	"encoding/json"
	"expvar"
	"fmt"
	"html/template"
	"log"
	"net/http"
	_ "net/http/pprof" // for pprof server
	"sync"

	raven "github.com/getsentry/raven-go"
	"github.com/julienschmidt/httprouter"
	"golang.org/x/sync/singleflight"

	"github.com/ndlib/bendo/blobcache"
	"github.com/ndlib/bendo/fragment"
	"github.com/ndlib/bendo/items"
	"github.com/ndlib/bendo/transaction"
)

// RESTServer holds the configuration for a Bendo REST API server.
//
// Set all the public fields and then call Run. Run will listen on the given
// port and handle requests. At the moment there is no maximum simultaneous
// request limit. Do not change any fields after calling Run.
//
// Run will also start a goroutine to handle serializing new file uploads
// into storage bags, and a goroutine to do fixity checking.
//
// There are two levels of configuration. It should be enough to only set
// CacheDir and Items. The other fields are exposed to allow more customization.
type RESTServer struct {
	// Port number to run bendo on. defaults to 14000
	PortNumber string

	// Port to run the profiler/inspector on. If empty, the goroutine is not
	// started. It provides information on the "/debug/" route.
	PProfPort string

	// Items is the base item store (that is, tape system or file system where
	// preservation content is stored). Run() will panic if Items is nil.
	Items *items.Store

	// --- The following fields are more advanced and only need to be
	// set in special situations. ---

	// Validator does authentication by validating any user tokens
	// presented to the API. If this is nil then no authentication will be
	// done.
	Validator TokenValidator

	// TxStore keeps information on transactions in progress. If this is
	// nil, transactions will be kept inside the cache directory.
	TxStore *transaction.Store

	// FileStore keeps the uploaded file waiting to be saved into the Item
	// store. If nil, the files will be stored inside the cache directory.
	FileStore *fragment.Store

	// Cache keeps smallish blobs retreived from tape.
	Cache blobcache.T

	// BlobDB holds the item/version/slot/blob information in a structured way
	// so we can query it without needing to read and parse the JSON structures
	// describing items.
	BlobDB BlobDB

	// Fixity stores the records tracking past and future fixity checks.
	FixityDatabase FixityDB
	DisableFixity  bool

	server   *http.Server   // used to close our listening socket
	txqueue  chan string    // channel to feed background transaction workers. contains tx ids
	txwg     sync.WaitGroup // for waiting for all background tx workers to exit
	txcancel chan struct{}  // Is closed to indicate tx workers should exit
	useTape  bool           // Is Bendo reading/writing from tape?

	// tapeinflight tracks whether a blob is being copied into the cache. If
	// one is, then a channel is returned that will signal when the copy is
	// finished. When that happens calling findContent() again will return
	// either a reader for the blob or the error that happened while copying it
	// into the cache.
	tapeinflight *singleflight.Group

	// errorledger tracks the errors that happen when copying blobs into the
	// cache. The errors are only kept for a short amount of time (at least
	// long enough that others waiting on the channel can call findContent
	// again to get the error).
	errorledger errorlist
}

// the number of transaction commits to tape we allow at a given time. If there
// are more they will wait in a queue.
const MaxConcurrentCommits = 2

// Run initializes and starts all the goroutines used by the server. It then
// blocks listening for and handling http requests.
func (s *RESTServer) Run() error {
	if s.Items == nil {
		log.Fatalln("No base storage given. Items is nil.")
	}

	if s.Validator == nil {
		log.Println("No Validator given")
		s.Validator = NobodyValidator{}
	}

	s.EnableTapeUse()

	if !s.DisableFixity {
		s.StartFixity()
	}

	// index the cached items into memory
	if s.Cache != nil {
		// not everything needs a scan. but if it does, run it
		// maybe Scan() should be added to the cache interface?
		type Scanner interface {
			Scan()
		}
		if c, ok := s.Cache.(Scanner); ok {
			go c.Scan()
		}
	}

	log.Println("Scanning Transactions")
	s.TxStore.Load()

	// init upload store
	log.Println("Scanning Upload Queue")
	s.FileStore.Load()

	log.Println("Starting Transaction Cleaner")
	go s.TxCleaner()

	log.Println("Starting pending transactions")
	s.txqueue = make(chan string, 100) // 100 is arbitrary. don't expect that many.
	s.txcancel = make(chan struct{})
	for i := 0; i < MaxConcurrentCommits; i++ {
		s.txwg.Add(1)
		go s.transactionWorker(s.txqueue)
	}
	go s.initCommitQueue() // run in background

	// for pprof
	if s.PProfPort != "" {
		log.Println("Starting PProf on port", s.PProfPort)
		go func() {
			log.Println("pprof:", http.ListenAndServe(":"+s.PProfPort, nil))
		}()
	}
	log.Println("Listening on", s.PortNumber)

	s.server = &http.Server{
		Handler: raven.Recoverer(s.addRoutes()),
		Addr:    ":" + s.PortNumber,
	}
	err := s.server.ListenAndServe()

	// being shutdown is not an error
	if err == http.ErrServerClosed {
		err = nil
	}
	return err
}

// Stop will stop the server and return when all the server goroutines have
// exited and the socked closed.
func (s *RESTServer) Stop() error {
	// first shutdown the transaction workers
	// We don't stop the fixity process. Should we?
	close(s.txcancel)
	s.txwg.Wait() // wait for all tx workers to exit

	// then shutdown all the HTTP connections
	return s.server.Shutdown(context.Background())
}

// initCommitQueue adds all transactions in the tx store to the transaction queue.
// It may block until they are all loaded and processed.
func (s *RESTServer) initCommitQueue() {
	// throw all transactions, even finished and errored ones, into
	// the queue. The transaction workers will sort it out.
	for _, tid := range s.TxStore.List() {
		select {
		case s.txqueue <- tid:
		case <-s.txcancel:
			return
		}
	}
}

func (s *RESTServer) Handler() http.Handler {
	return s.addRoutes()
}

func (s *RESTServer) addRoutes() http.Handler {
	var routes = []struct {
		method  string
		route   string
		role    Role // RoleUnknown means no API key is needed to access
		handler httprouter.Handle
	}{
		{"GET", "/item/:id/*slot", RoleUnknown, s.SlotHandler},
		{"HEAD", "/item/:id/*slot", RoleUnknown, s.SlotHandler},
		{"GET", "/item/:id", RoleUnknown, s.ItemHandler},

		// all the transaction things.
		{"POST", "/item/:id/transaction", RoleWrite, s.NewTxHandler},
		{"GET", "/transaction", RoleRead, s.ListTxHandler},
		{"GET", "/transaction/:tid", RoleRead, s.TxInfoHandler},
		{"POST", "/transaction/:tid/cancel", RoleWrite, s.CancelTxHandler}, //keep?

		// file upload things
		{"GET", "/upload", RoleRead, s.ListFileHandler},
		{"POST", "/upload", RoleWrite, s.AppendFileHandler},
		{"GET", "/upload/:fileid", RoleRead, s.GetFileHandler},
		{"POST", "/upload/:fileid", RoleWrite, s.AppendFileHandler},
		{"DELETE", "/upload/:fileid", RoleWrite, s.DeleteFileHandler},
		{"GET", "/upload/:fileid/metadata", RoleMDOnly, s.GetFileInfoHandler},
		{"PUT", "/upload/:fileid/metadata", RoleWrite, s.SetFileInfoHandler},

		// fixity routes
		{"GET", "/fixity", RoleRead, s.GetFixityHandler},
		{"GET", "/fixity/:id", RoleRead, s.GetFixityIdHandler},
		{"POST", "/fixity/:item", RoleWrite, s.PostFixityHandler},
		{"PUT", "/fixity/:id", RoleWrite, s.PutFixityHandler},
		{"DELETE", "/fixity/:id", RoleWrite, s.DeleteFixityHandler},

		// /admin/tape_use (enable, disable, get status)
		{"GET", "/admin/use_tape", RoleUnknown, s.GetTapeUseHandler},
		{"PUT", "/admin/use_tape/:status", RoleAdmin, s.SetTapeUseHandler},

		// the read only bundle stuff
		{"GET", "/bundle/list/:prefix", RoleRead, s.BundleListPrefixHandler},
		{"GET", "/bundle/list/", RoleRead, s.BundleListHandler},
		{"GET", "/bundle/open/:key", RoleRead, s.BundleOpenHandler},

		// UI routes.
		// these routes are not covered by the API spec and can change at any time
		{"GET", "/ui/items", RoleUnknown, s.UIItemsHandler},

		// other
		{"GET", "/", RoleUnknown, WelcomeHandler},
		{"GET", "/stats", RoleUnknown, NotImplementedHandler},
		{"GET", "/debug/vars", RoleUnknown, VarHandler}, // standard route for expvars data
	}

	r := httprouter.New()
	for _, route := range routes {
		r.Handle(route.method,
			route.route,
			logWrapper(s.authzWrapper(route.handler, route.role)))
	}
	return r
}

// General route handlers and convinence functions

// VarHandler adapts the expvar default handler to the httprouter three parameter handler.
func VarHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	// this code is taken from the stdlib expvar package.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	fmt.Fprintf(w, "{\n")
	first := true
	expvar.Do(func(kv expvar.KeyValue) {
		if !first {
			fmt.Fprintf(w, ",\n")
		}
		first = false
		fmt.Fprintf(w, "%q: %s", kv.Key, kv.Value)
	})
	fmt.Fprintf(w, "\n}\n")
}

// NotImplementedHandler will return a 501 not implemented error.
func NotImplementedHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	w.WriteHeader(http.StatusNotImplemented)
	fmt.Fprintf(w, "Not Implemented\n")
}

// writeHTMLorJSON will either return val as JSON or as rendered using the
// given template, depending on the request header "Accept-Encoding".
func writeHTMLorJSON(w http.ResponseWriter,
	r *http.Request,
	tmpl *template.Template,
	val interface{}) {

	if r.Header.Get("Accept-Encoding") == "application/json" ||
		r.FormValue("format") == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(val)
		return
	}
	err := tmpl.Execute(w, val)
	if err != nil {
		log.Println(err)
		raven.CaptureError(err, nil)
	}
}

// authzWrapper returns a Handler which will first verify the user token as
// having at least the given Role. The user name is added as a parameter
// "username".
func (s *RESTServer) authzWrapper(handler httprouter.Handle, leastRole Role) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		// the token may be passed in either the X-Api-Key header, or as the username
		// or the password in basic auth (to support human use)
		token := r.Header.Get("X-Api-Key")
		if token == "" {
			// token in username field?
			token, _, _ = r.BasicAuth()
		}
		if token == "" {
			// token in password field?
			_, token, _ = r.BasicAuth()
		}
		user, role, err := s.Validator.TokenValid(token)
		if err != nil {
			w.WriteHeader(500)
			fmt.Fprintln(w, err.Error())
			return
		}

		// is role valid?
		if role < leastRole {
			w.Header().Set("WWW-Authenticate", "Basic") // tell web browsers to display password box
			w.WriteHeader(401)
			fmt.Fprintln(w, "Forbidden")
			return
		}

		log.Println("User", user)

		// remove any previous username
		for i := range ps {
			if ps[i].Key == "username" {
				ps[i].Value = user
				goto out
			}
		}
		// add a new username if none found
		ps = append(ps, httprouter.Param{Key: "username", Value: user})
	out:
		handler(w, r, ps)
	}
}

// logWrapper takes a handler and returns a handler which does the same thing,
// after first logging the request URL.
func logWrapper(handler httprouter.Handle) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		log.Println(r.Method, r.URL)
		handler(w, r, ps)
	}
}

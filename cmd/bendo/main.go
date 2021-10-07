package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/ndlib/bendo/blobcache"
	"github.com/ndlib/bendo/fragment"
	"github.com/ndlib/bendo/items"
	"github.com/ndlib/bendo/server"
	"github.com/ndlib/bendo/store"
	"github.com/ndlib/bendo/transaction"
	// logs all http requests. useful for debugging S3
	//	_ "github.com/motemen/go-loghttp/global"
)

//Config info needed for Bendo

type bendoConfig struct {
	StoreDir     string
	Tokenfile    string
	CacheDir     string
	CacheSize    int64
	CacheTimeout string
	PortNumber   string
	PProfPort    string
	Mysql        string
	CowHost      string
	CowToken     string
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// Start with the Default values
	config := &bendoConfig{
		StoreDir:     ".",
		Tokenfile:    "",
		CacheDir:     "",
		CacheSize:    100,
		CacheTimeout: "",
		PortNumber:   "14000",
		PProfPort:    "14001",
		Mysql:        "",
		CowHost:      "",
		CowToken:     "",
	}

	var configFile = flag.String("config-file", "", "Configuration File")
	flag.Parse()
	// If config file name is provided, try to open & decode it
	if *configFile != "" {
		log.Printf("Using config file %s\n", *configFile)
		if _, err := toml.DecodeFile(*configFile, config); err != nil {
			log.Println(err)
			return
		}
	}

	log.Println("==========")
	log.Println("Starting Bendo Server version", server.Version)
	log.Println("StoreDir =", config.StoreDir)
	log.Println("CacheDir =", config.CacheDir)
	log.Println("CacheSize =", config.CacheSize)
	log.Println("CacheTimeout =", config.CacheTimeout)

	// use the config values to set up the server
	var s = &server.RESTServer{
		Items:      nil,
		Validator:  nil,
		PortNumber: config.PortNumber,
		PProfPort:  config.PProfPort,
	}

	// Use the config settings to update s.
	// All the setup* functions panic on error.
	setupTokens(config, s)
	// set up preservation store. Do this before setting up the database.
	setupItemStore(config, s)
	setupCache(config, s)
	setupTransactionStore(config, s)
	setupUploadStore(config, s)
	setupDatabase(config, s)

	// install signal handlers
	sig := make(chan os.Signal, 5)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go signalHandler(sig, s)

	err := s.Run()
	if err != nil {
		log.Println(err)
	}
	log.Println("Exiting")
}

func signalHandler(sig <-chan os.Signal, svr *server.RESTServer) {
	for s := range sig {
		log.Println("---Received signal", s)
		switch s {
		case syscall.SIGINT, syscall.SIGTERM:
			svr.Stop() // this will cause Run to exit
		}
	}
}

// setupItemStore uses config to mutate s to add the item store.
// It will panic on error.
func setupItemStore(config *bendoConfig, s *server.RESTServer) {
	itemstore := parselocation(config.StoreDir, "")
	if itemstore == nil {
		log.Fatalln("no storage location")
	}
	if config.CowHost != "" {
		log.Printf("Using COW with target %s", config.CowHost)
		itemstore = store.NewCOW(itemstore, config.CowHost, config.CowToken)

		// don't run fixity if we are using a copy-on-write.
		// (doing so will cause us to download ALL the data from
		// the target bendo over time)
		s.DisableFixity = true
	}
	s.Items = items.New(itemstore)
}

// setupTokens configures the token verification. It will panic on error.
func setupTokens(config *bendoConfig, s *server.RESTServer) {
	if config.Tokenfile != "" {
		var err error
		log.Printf("Using user token file %s\n", config.Tokenfile)
		s.Validator, err = server.NewListValidatorFile(config.Tokenfile)
		if err != nil {
			log.Fatalln(err)
		}
	} else {
		log.Printf("No user token file specified")
		s.Validator = server.NobodyValidator{}
	}
}

func setupCache(config *bendoConfig, s *server.RESTServer) {
	timeout, _ := time.ParseDuration(config.CacheTimeout)
	size := config.CacheSize * 1000000 // config is in MB
	if config.CacheDir == "" || size == 0 {
		log.Println("Not using blob cache")
		s.Cache = blobcache.EmptyCache{}
	} else {
		v := parselocation(config.CacheDir, "blobcache")
		if v == nil {
			log.Fatalln("no location for cache")
		}
		if timeout != 0 {
			log.Println("Using time-based cache strategy")
			s.Cache = blobcache.NewTime(v, timeout)
		} else {
			log.Println("Using size-based cache strategy")
			s.Cache = blobcache.NewLRU(v, size)
		}
	}
}

func setupTransactionStore(config *bendoConfig, s *server.RESTServer) {
	v := parselocation(config.CacheDir, "transaction")
	s.TxStore = transaction.New(v)
}

func setupUploadStore(config *bendoConfig, s *server.RESTServer) {
	v := parselocation(config.CacheDir, "upload")
	s.FileStore = fragment.New(v)
}

func setupDatabase(config *bendoConfig, s *server.RESTServer) {
	var db interface {
		server.FixityDB
		items.ItemCache
		server.BlobDB
	}
	var err error
	if config.Mysql != "" {
		log.Printf("Using MySQL")
		db, err = server.NewMysqlCache(config.Mysql)
	} else {
		var path string
		// this gets wonky if the cacheDir is an s3: path. but it still works!
		// (it makes a file system directory named "s3:")
		if config.CacheDir != "" {
			os.MkdirAll(config.CacheDir, 0755)
			path = filepath.Join(config.CacheDir, "bendo.ql")
		} else {
			path = "memory"
		}
		log.Println("Using internal database at", path)
		db, err = server.NewQlCache(path)
	}
	if db == nil || err != nil {
		log.Fatalln("problem setting up database")
	}
	s.BlobDB = db
	s.FixityDatabase = db
	s.Items.SetCache(db)
}

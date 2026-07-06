// objectstorage is a small object storage server for a NAS: buckets are
// directories, object metadata lives in a per-bucket SQLite database, and
// object content is stored content-addressed by sha256.
//
// It speaks (a subset of) the Google Cloud Storage JSON API; point the
// official clients at it with STORAGE_EMULATOR_HOST=<host>:<port>.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/justinsb/objectstorage/pkg/gcs"
	"github.com/justinsb/objectstorage/pkg/store"
)

func main() {
	dataDir := flag.String("data-dir", "data", "directory holding bucket data")
	listen := flag.String("listen", ":8080", "address to listen on")
	flag.Parse()

	st, err := store.Open(*dataDir)
	if err != nil {
		log.Fatalf("opening store at %s: %v", *dataDir, err)
	}
	defer st.Close()

	server := &http.Server{
		Addr:    *listen,
		Handler: gcs.NewServer(st),
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		log.Printf("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}()

	log.Printf("objectstorage serving %s on %s", *dataDir, *listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

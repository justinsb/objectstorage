// objectstorage is a small object storage server for a NAS: buckets are
// directories, object metadata lives in a per-bucket SQLite database, and
// object content is stored content-addressed by sha256.
//
// It speaks two protocols over the same store, on separate ports:
//   - Google Cloud Storage JSON API (point clients at it with
//     STORAGE_EMULATOR_HOST=<host>:<gcs-port>)
//   - Amazon S3 (path-style; set the SDK endpoint to <host>:<s3-port>)
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/justinsb/objectstorage/pkg/gcs"
	"github.com/justinsb/objectstorage/pkg/s3"
	"github.com/justinsb/objectstorage/pkg/store"
)

func main() {
	dataDir := flag.String("data-dir", "data", "directory holding bucket data")
	listen := flag.String("listen", ":8080", "address for the GCS JSON API")
	s3Listen := flag.String("s3-listen", ":8081", "address for the S3 API (empty to disable)")
	flag.Parse()

	st, err := store.Open(*dataDir)
	if err != nil {
		log.Fatalf("opening store at %s: %v", *dataDir, err)
	}
	defer st.Close()

	servers := []*http.Server{
		{Addr: *listen, Handler: gcs.NewServer(st)},
	}
	if *s3Listen != "" {
		servers = append(servers, &http.Server{Addr: *s3Listen, Handler: s3.NewServer(st)})
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		log.Printf("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, srv := range servers {
			srv.Shutdown(ctx)
		}
	}()

	log.Printf("objectstorage serving %s: GCS on %s, S3 on %s", *dataDir, *listen, *s3Listen)
	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatal(err)
			}
		}()
	}
	wg.Wait()
}

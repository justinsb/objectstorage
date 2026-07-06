// Package e2e tests the server end-to-end through the official
// cloud.google.com/go/storage client. The server runs in-process on a random
// port, so the suite is hermetic and runs on a plain `go test ./...`.
package e2e

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/justinsb/objectstorage/pkg/gcs"
	"github.com/justinsb/objectstorage/pkg/s3"
	"github.com/justinsb/objectstorage/pkg/store"

	"net/http/httptest"
)

// ProjectID is the fake project used for bucket creation; the server ignores
// it, but the client API requires one.
const ProjectID = "test-project"

// Harness runs the server in-process and provides official GCS and S3
// clients pointed at it.
type Harness struct {
	T        *testing.T
	Client   *storage.Client
	S3Client *awss3.Client
	DataDir  string
	BaseURL  string
	S3URL    string
}

// NewHarness starts a fresh server on a random port with an empty data
// directory, and connects the official GCS Go client to it via
// STORAGE_EMULATOR_HOST. Everything is cleaned up when the test ends.
func NewHarness(t *testing.T) *Harness {
	t.Helper()
	dataDir := t.TempDir()

	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	server := httptest.NewServer(gcs.NewServer(st))
	s3Server := httptest.NewServer(s3.NewServer(st))
	t.Cleanup(func() {
		server.Close()
		s3Server.Close()
		st.Close()
	})

	// t.Setenv also guards against this test running in parallel with
	// other tests in the package, which would race on the env var.
	t.Setenv("STORAGE_EMULATOR_HOST", strings.TrimPrefix(server.URL, "http://"))

	client, err := storage.NewClient(context.Background())
	if err != nil {
		t.Fatalf("creating storage client: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	s3Client := awss3.New(awss3.Options{
		BaseEndpoint: aws.String(s3Server.URL),
		Region:       "us-east-1",
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
	})

	return &Harness{
		T: t, Client: client, S3Client: s3Client,
		DataDir: dataDir, BaseURL: server.URL, S3URL: s3Server.URL,
	}
}

// MustCreateBucket creates a bucket or fails the test.
func (h *Harness) MustCreateBucket(ctx context.Context, name string) *storage.BucketHandle {
	h.T.Helper()
	bkt := h.Client.Bucket(name)
	if err := bkt.Create(ctx, ProjectID, nil); err != nil {
		h.T.Fatalf("creating bucket %q: %v", name, err)
	}
	return bkt
}

// CountBlobFiles counts the blob files stored in a bucket's CAS directory,
// for verifying deduplication and garbage collection.
func (h *Harness) CountBlobFiles(bucket string) int {
	h.T.Helper()
	count := 0
	root := filepath.Join(h.DataDir, bucket, "objects")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			count++
		}
		return nil
	})
	if err != nil {
		h.T.Fatalf("walking blob dir: %v", err)
	}
	return count
}

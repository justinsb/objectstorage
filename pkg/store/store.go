// Package store implements the storage backend: one directory per bucket,
// object metadata in a per-bucket SQLite database, and object contents in a
// content-addressed (sha256) blob store shared within the bucket.
//
// Layout on disk:
//
//	<root>/<bucket>/data.sqlite
//	<root>/<bucket>/objects/<hh>/<sha256-hex>
//	<root>/<bucket>/tmp/
//
// Exactly one Store (one process) may own a root directory at a time.
package store

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
)

// Store is the root of the object storage backend.
type Store struct {
	root string

	mu      sync.Mutex
	buckets map[string]*Bucket // open bucket handles
}

// Open opens (creating if needed) a store rooted at the given directory.
func Open(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("creating store root: %w", err)
	}
	return &Store{root: root, buckets: make(map[string]*Bucket)}, nil
}

// Close closes all open bucket databases.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for name, b := range s.buckets {
		if err := b.db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(s.buckets, name)
	}
	return firstErr
}

// Bucket names follow the (simplified) GCS/S3 rules: lowercase letters,
// digits, dashes, underscores and dots; must start and end with a letter or
// digit; 3-63 characters.
var validBucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,61}[a-z0-9]$`)

// CreateBucket creates a new bucket directory and its metadata database.
func (s *Store) CreateBucket(name string) (*BucketInfo, error) {
	if !validBucketName.MatchString(name) {
		return nil, fmt.Errorf("%w: bucket %q", ErrInvalidName, name)
	}
	dir := filepath.Join(s.root, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		if os.IsExist(err) {
			return nil, ErrBucketExists
		}
		return nil, err
	}
	for _, sub := range []string{"objects", "tmp"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	b, err := s.openBucket(name)
	if err != nil {
		return nil, err
	}
	return b.info()
}

// GetBucket returns metadata for a bucket.
func (s *Store) GetBucket(name string) (*BucketInfo, error) {
	b, err := s.bucket(name)
	if err != nil {
		return nil, err
	}
	return b.info()
}

// ListBuckets returns all buckets, sorted by name.
func (s *Store) ListBuckets() ([]BucketInfo, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var infos []BucketInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.root, e.Name(), "data.sqlite")); err != nil {
			continue // not a bucket directory
		}
		b, err := s.bucket(e.Name())
		if err != nil {
			return nil, err
		}
		info, err := b.info()
		if err != nil {
			return nil, err
		}
		infos = append(infos, *info)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos, nil
}

// DeleteBucket deletes a bucket; the bucket must be empty.
func (s *Store) DeleteBucket(name string) error {
	b, err := s.bucket(name)
	if err != nil {
		return err
	}
	b.commitMu.Lock()
	defer b.commitMu.Unlock()

	var count int64
	if err := b.db.QueryRow(`SELECT COUNT(*) FROM objects`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return ErrBucketNotEmpty
	}

	s.mu.Lock()
	delete(s.buckets, name)
	s.mu.Unlock()
	if err := b.db.Close(); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(s.root, name))
}

// bucket returns the open handle for an existing bucket.
func (s *Store) bucket(name string) (*Bucket, error) {
	if !validBucketName.MatchString(name) {
		return nil, ErrBucketNotFound
	}
	s.mu.Lock()
	if b, ok := s.buckets[name]; ok {
		s.mu.Unlock()
		return b, nil
	}
	s.mu.Unlock()

	if _, err := os.Stat(filepath.Join(s.root, name, "data.sqlite")); err != nil {
		return nil, ErrBucketNotFound
	}
	return s.openBucket(name)
}

func (s *Store) openBucket(name string) (*Bucket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.buckets[name]; ok {
		return b, nil
	}
	b, err := openBucket(name, filepath.Join(s.root, name))
	if err != nil {
		return nil, err
	}
	s.buckets[name] = b
	return b, nil
}

package store

import (
	"crypto/md5"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"time"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// BlobWriter streams object content into the store, hashing as it goes.
// Content is written to a temp file, then committed into the CAS under its
// sha256 together with the object metadata row, or aborted.
//
// A BlobWriter is not safe for concurrent use.
type BlobWriter struct {
	b    *Bucket
	tmp  *os.File
	sha  hash.Hash
	md5h hash.Hash
	crc  hash.Hash32
	size int64
	done bool
}

// NewBlobWriter starts a new object upload into the given bucket.
func (s *Store) NewBlobWriter(bucket string) (*BlobWriter, error) {
	b, err := s.bucket(bucket)
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(b.dir+"/tmp", "upload-*")
	if err != nil {
		return nil, err
	}
	return &BlobWriter{
		b:    b,
		tmp:  tmp,
		sha:  sha256.New(),
		md5h: md5.New(),
		crc:  crc32.New(castagnoli),
	}, nil
}

func (w *BlobWriter) Write(p []byte) (int, error) {
	if w.done {
		return 0, errors.New("write on finished BlobWriter")
	}
	n, err := w.tmp.Write(p)
	if n > 0 {
		w.sha.Write(p[:n])
		w.md5h.Write(p[:n])
		w.crc.Write(p[:n])
		w.size += int64(n)
	}
	return n, err
}

// Size returns the number of bytes written so far.
func (w *BlobWriter) Size() int64 { return w.size }

// Checksums returns the checksums of the content written so far.
func (w *BlobWriter) Checksums() (sha256Hex string, md5Sum []byte, crc32c uint32) {
	return hex.EncodeToString(w.sha.Sum(nil)), w.md5h.Sum(nil), w.crc.Sum32()
}

// Abort discards the upload.
func (w *BlobWriter) Abort() {
	if w.done {
		return
	}
	w.done = true
	name := w.tmp.Name()
	w.tmp.Close()
	os.Remove(name)
}

// Commit atomically installs the content as object `name`, subject to
// opts.Conditions. On success the temp file has been moved (or deduplicated)
// into the CAS and the metadata row is visible.
func (w *BlobWriter) Commit(name string, opts PutOptions) (*ObjectInfo, error) {
	if w.done {
		return nil, errors.New("commit on finished BlobWriter")
	}
	w.done = true
	tmpName := w.tmp.Name()
	defer os.Remove(tmpName) // no-op if we renamed it away

	if !validObjectName(name) {
		w.tmp.Close()
		return nil, fmt.Errorf("%w: object %q", ErrInvalidName, name)
	}
	if err := w.tmp.Sync(); err != nil {
		w.tmp.Close()
		return nil, err
	}
	if err := w.tmp.Close(); err != nil {
		return nil, err
	}

	shaHex, md5Sum, crc32c := w.Checksums()
	b := w.b

	b.commitMu.Lock()
	defer b.commitMu.Unlock()

	// Install the blob file. If it already exists we are deduplicating;
	// the refcount is adjusted in the transaction below.
	blobPath := b.blobPath(shaHex)
	if _, err := os.Stat(blobPath); err != nil {
		if err := os.MkdirAll(blobPath[:len(blobPath)-len(shaHex)-1], 0o755); err != nil {
			return nil, err
		}
		if err := os.Rename(tmpName, blobPath); err != nil {
			return nil, err
		}
	}

	tx, err := b.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	existing, err := b.scanObject(tx.QueryRow(`SELECT `+objectColumns+` FROM objects WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		existing = nil
	} else if err != nil {
		return nil, err
	}
	if err := opts.Conditions.check(existing); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	generation := now.UnixMicro()
	if existing != nil && existing.Generation >= generation {
		generation = existing.Generation + 1
	}

	if _, err := tx.Exec(
		`INSERT INTO blobs (sha256, size, refcount) VALUES (?, ?, 1)
		 ON CONFLICT(sha256) DO UPDATE SET refcount = refcount + 1`,
		shaHex, w.size); err != nil {
		return nil, err
	}
	var orphan string
	if existing != nil {
		orphaned, err := decrefBlob(tx, existing.SHA256)
		if err != nil {
			return nil, err
		}
		if orphaned {
			orphan = existing.SHA256
		}
	}

	metadataJSON := "{}"
	if len(opts.Metadata) > 0 {
		buf, err := json.Marshal(opts.Metadata)
		if err != nil {
			return nil, err
		}
		metadataJSON = string(buf)
	}

	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO objects
		 (name, generation, metageneration, sha256, size, md5, crc32c, content_type, metadata, created, updated)
		 VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?)`,
		name, generation, shaHex, w.size, md5Sum, int64(crc32c),
		opts.ContentType, metadataJSON, now.UnixMicro(), now.UnixMicro()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if orphan != "" {
		os.Remove(b.blobPath(orphan))
	}

	return &ObjectInfo{
		Bucket:         b.name,
		Name:           name,
		Generation:     generation,
		Metageneration: 1,
		Size:           w.size,
		SHA256:         shaHex,
		MD5:            md5Sum,
		CRC32C:         crc32c,
		ContentType:    opts.ContentType,
		Metadata:       opts.Metadata,
		Created:        now,
		Updated:        now,
	}, nil
}

// TempFile creates a temporary file in the bucket's tmp directory, for
// staging data (e.g. multipart upload parts) that will later be streamed
// into a BlobWriter. The caller owns the file and must remove it.
func (s *Store) TempFile(bucket string) (*os.File, error) {
	b, err := s.bucket(bucket)
	if err != nil {
		return nil, err
	}
	return os.CreateTemp(filepath.Join(b.dir, "tmp"), "part-*")
}

// PutObject writes a complete object from r. It is a convenience wrapper
// around NewBlobWriter + Commit.
func (s *Store) PutObject(bucket, name string, r io.Reader, opts PutOptions) (*ObjectInfo, error) {
	w, err := s.NewBlobWriter(bucket)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(w, r); err != nil {
		w.Abort()
		return nil, err
	}
	return w.Commit(name, opts)
}

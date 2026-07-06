package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Bucket is an open handle to a bucket's metadata database and blob store.
type Bucket struct {
	name string
	dir  string
	db   *sql.DB

	// commitMu serializes mutations (object commits, deletes, bucket
	// delete). SQLite serializes writes anyway; holding this across the
	// whole commit (including blob file rename/removal) keeps the CAS
	// refcounts and the files on disk consistent with each other.
	commitMu sync.Mutex
}

const schema = `
CREATE TABLE IF NOT EXISTS bucket_info (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	created INTEGER NOT NULL,        -- unix micros
	metageneration INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS objects (
	name TEXT PRIMARY KEY,
	generation INTEGER NOT NULL,
	metageneration INTEGER NOT NULL,
	sha256 TEXT NOT NULL,
	size INTEGER NOT NULL,
	md5 BLOB NOT NULL,
	crc32c INTEGER NOT NULL,
	content_type TEXT NOT NULL DEFAULT '',
	metadata TEXT NOT NULL DEFAULT '{}', -- JSON object
	created INTEGER NOT NULL,            -- unix micros
	updated INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS blobs (
	sha256 TEXT PRIMARY KEY,
	size INTEGER NOT NULL,
	refcount INTEGER NOT NULL
);
`

func openBucket(name, dir string) (*Bucket, error) {
	dsn := "file:" + filepath.Join(dir, "data.sqlite") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening bucket db: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initializing bucket schema: %w", err)
	}
	if _, err := db.Exec(
		`INSERT OR IGNORE INTO bucket_info (id, created, metageneration) VALUES (1, ?, 1)`,
		time.Now().UnixMicro()); err != nil {
		db.Close()
		return nil, err
	}
	return &Bucket{name: name, dir: dir, db: db}, nil
}

func (b *Bucket) info() (*BucketInfo, error) {
	var created, metagen int64
	if err := b.db.QueryRow(`SELECT created, metageneration FROM bucket_info WHERE id = 1`).
		Scan(&created, &metagen); err != nil {
		return nil, err
	}
	return &BucketInfo{
		Name:           b.name,
		Created:        time.UnixMicro(created).UTC(),
		Metageneration: metagen,
	}, nil
}

func validObjectName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, "\x00\n\r") && len(name) <= 1024
}

func (b *Bucket) blobPath(sha256Hex string) string {
	return filepath.Join(b.dir, "objects", sha256Hex[:2], sha256Hex)
}

const objectColumns = `name, generation, metageneration, sha256, size, md5, crc32c, content_type, metadata, created, updated`

func (b *Bucket) scanObject(row interface{ Scan(...any) error }) (*ObjectInfo, error) {
	var info ObjectInfo
	var metadataJSON string
	var created, updated int64
	err := row.Scan(&info.Name, &info.Generation, &info.Metageneration, &info.SHA256,
		&info.Size, &info.MD5, &info.CRC32C, &info.ContentType, &metadataJSON, &created, &updated)
	if err != nil {
		return nil, err
	}
	info.Bucket = b.name
	info.Created = time.UnixMicro(created).UTC()
	info.Updated = time.UnixMicro(updated).UTC()
	if metadataJSON != "" && metadataJSON != "{}" {
		if err := json.Unmarshal([]byte(metadataJSON), &info.Metadata); err != nil {
			return nil, fmt.Errorf("corrupt metadata for object %q: %w", info.Name, err)
		}
	}
	return &info, nil
}

// GetObject returns metadata for the live version of an object.
func (s *Store) GetObject(bucket, name string) (*ObjectInfo, error) {
	b, err := s.bucket(bucket)
	if err != nil {
		return nil, err
	}
	return b.getObject(name)
}

func (b *Bucket) getObject(name string) (*ObjectInfo, error) {
	row := b.db.QueryRow(`SELECT `+objectColumns+` FROM objects WHERE name = ?`, name)
	info, err := b.scanObject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrObjectNotFound
	}
	return info, err
}

// OpenObject returns object metadata plus an open file handle on its content.
// The caller must close the file.
func (s *Store) OpenObject(bucket, name string) (*ObjectInfo, *os.File, error) {
	b, err := s.bucket(bucket)
	if err != nil {
		return nil, nil, err
	}
	info, err := b.getObject(name)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(b.blobPath(info.SHA256))
	if err != nil {
		return nil, nil, fmt.Errorf("opening blob for %s/%s: %w", bucket, name, err)
	}
	return info, f, nil
}

// DeleteObject deletes the live version of an object, subject to conditions.
func (s *Store) DeleteObject(bucket, name string, conds Conditions) error {
	b, err := s.bucket(bucket)
	if err != nil {
		return err
	}
	b.commitMu.Lock()
	defer b.commitMu.Unlock()

	tx, err := b.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	existing, err := b.scanObject(tx.QueryRow(`SELECT `+objectColumns+` FROM objects WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		existing = nil
	} else if err != nil {
		return err
	}
	if err := conds.check(existing); err != nil {
		return err
	}
	if existing == nil {
		return ErrObjectNotFound
	}
	if _, err := tx.Exec(`DELETE FROM objects WHERE name = ?`, name); err != nil {
		return err
	}
	orphan, err := decrefBlob(tx, existing.SHA256)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if orphan {
		os.Remove(b.blobPath(existing.SHA256))
	}
	return nil
}

// decrefBlob decrements a blob's refcount inside tx, deleting the row when it
// reaches zero. It reports whether the blob file is now orphaned; the caller
// removes the file after a successful commit (while holding commitMu).
func decrefBlob(tx *sql.Tx, sha256Hex string) (orphaned bool, err error) {
	if _, err := tx.Exec(`UPDATE blobs SET refcount = refcount - 1 WHERE sha256 = ?`, sha256Hex); err != nil {
		return false, err
	}
	var refcount int64
	err = tx.QueryRow(`SELECT refcount FROM blobs WHERE sha256 = ?`, sha256Hex).Scan(&refcount)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if refcount <= 0 {
		if _, err := tx.Exec(`DELETE FROM blobs WHERE sha256 = ?`, sha256Hex); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

const defaultMaxResults = 1000

// prefixSuccessor returns the smallest string greater than every string with
// the given prefix, or "" if there is none (prefix is all 0xff).
func prefixSuccessor(prefix string) string {
	for i := len(prefix) - 1; i >= 0; i-- {
		if prefix[i] != 0xff {
			return prefix[:i] + string(prefix[i]+1)
		}
	}
	return ""
}

// ListObjects lists live objects with GCS list semantics (prefix, delimiter,
// pagination). PageToken is an inclusive lower bound on the object name.
func (s *Store) ListObjects(bucket string, req ListRequest) (*ListResult, error) {
	b, err := s.bucket(bucket)
	if err != nil {
		return nil, err
	}

	maxResults := req.MaxResults
	if maxResults <= 0 || maxResults > defaultMaxResults {
		maxResults = defaultMaxResults
	}

	// next is the inclusive lower bound for the scan.
	next := req.Prefix
	if req.PageToken > next {
		next = req.PageToken
	}
	end := prefixSuccessor(req.Prefix) // exclusive upper bound; "" = unbounded

	result := &ListResult{}
	count := 0
	const batchSize = 1000

scan:
	for {
		query := `SELECT ` + objectColumns + ` FROM objects WHERE name >= ?`
		args := []any{next}
		if end != "" {
			query += ` AND name < ?`
			args = append(args, end)
		}
		query += ` ORDER BY name LIMIT ?`
		args = append(args, batchSize)

		rows, err := b.db.Query(query, args...)
		if err != nil {
			return nil, err
		}
		var batch []*ObjectInfo
		for rows.Next() {
			info, err := b.scanObject(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			batch = append(batch, info)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}

		for _, info := range batch {
			if count >= maxResults {
				result.NextPageToken = info.Name
				break scan
			}
			rest := info.Name[len(req.Prefix):]
			if req.Delimiter != "" {
				if idx := strings.Index(rest, req.Delimiter); idx >= 0 {
					cp := req.Prefix + rest[:idx+len(req.Delimiter)]
					result.Prefixes = append(result.Prefixes, cp)
					count++
					// Skip the rest of this common prefix.
					next = prefixSuccessor(cp)
					if next == "" {
						break scan
					}
					continue scan
				}
			}
			result.Objects = append(result.Objects, *info)
			count++
		}
		if len(batch) < batchSize {
			break
		}
		next = batch[len(batch)-1].Name + "\x00"
	}
	return result, nil
}

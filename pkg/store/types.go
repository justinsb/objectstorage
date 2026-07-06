package store

import (
	"errors"
	"time"
)

var (
	ErrBucketNotFound     = errors.New("bucket not found")
	ErrBucketExists       = errors.New("bucket already exists")
	ErrBucketNotEmpty     = errors.New("bucket not empty")
	ErrObjectNotFound     = errors.New("object not found")
	ErrPreconditionFailed = errors.New("precondition failed")
	ErrInvalidName        = errors.New("invalid name")
)

// BucketInfo describes a bucket.
type BucketInfo struct {
	Name           string
	Created        time.Time
	Metageneration int64
}

// ObjectInfo describes a single (live) object version.
type ObjectInfo struct {
	Bucket         string
	Name           string
	Generation     int64
	Metageneration int64
	Size           int64
	SHA256         string // hex
	MD5            []byte // 16 bytes
	CRC32C         uint32 // Castagnoli
	ContentType    string
	Metadata       map[string]string
	Created        time.Time
	Updated        time.Time
}

// Conditions are GCS-style preconditions, evaluated inside the commit
// transaction. A nil field means "no condition".
type Conditions struct {
	GenerationMatch        *int64 // 0 means "object must not exist"
	GenerationNotMatch     *int64
	MetagenerationMatch    *int64
	MetagenerationNotMatch *int64
}

func (c Conditions) check(existing *ObjectInfo) error {
	var gen, metagen int64
	if existing != nil {
		gen = existing.Generation
		metagen = existing.Metageneration
	}
	if c.GenerationMatch != nil && gen != *c.GenerationMatch {
		return ErrPreconditionFailed
	}
	if c.GenerationNotMatch != nil && existing != nil && gen == *c.GenerationNotMatch {
		return ErrPreconditionFailed
	}
	if c.MetagenerationMatch != nil && metagen != *c.MetagenerationMatch {
		return ErrPreconditionFailed
	}
	if c.MetagenerationNotMatch != nil && existing != nil && metagen == *c.MetagenerationNotMatch {
		return ErrPreconditionFailed
	}
	return nil
}

// PutOptions carries object metadata and preconditions for a write.
type PutOptions struct {
	ContentType string
	Metadata    map[string]string
	Conditions  Conditions
}

// ListRequest is a request to list objects within a bucket.
type ListRequest struct {
	Prefix     string
	Delimiter  string
	PageToken  string // inclusive lower bound on object name; opaque to callers
	MaxResults int    // <=0 means default (1000)
}

// ListResult is one page of list results.
type ListResult struct {
	Objects       []ObjectInfo
	Prefixes      []string
	NextPageToken string
}

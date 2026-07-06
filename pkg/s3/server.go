// Package s3 implements (a useful subset of) the Amazon S3 API on top of the
// store package: bucket and object CRUD, ListObjects V1/V2, multipart
// uploads, batch delete, and server-side copy. Path-style addressing only.
//
// Authentication is intentionally not enforced (trusted-LAN NAS assumption);
// SigV4 headers and aws-chunked bodies are accepted and ignored/decoded.
package s3

import (
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/justinsb/objectstorage/pkg/store"
)

// Server is an http.Handler implementing the S3 API.
type Server struct {
	store *store.Store

	mu      sync.Mutex
	uploads map[string]*multipartUpload // in-progress multipart uploads by uploadId
}

func NewServer(st *store.Store) *Server {
	return &Server{store: st, uploads: make(map[string]*multipartUpload)}
}

// splitPath splits an escaped URL path into unescaped segments.
func splitPath(escapedPath string) ([]string, error) {
	trimmed := strings.Trim(escapedPath, "/")
	if trimmed == "" {
		return nil, nil
	}
	parts := strings.Split(trimmed, "/")
	segs := make([]string, 0, len(parts))
	for _, p := range parts {
		seg, err := url.PathUnescape(p)
		if err != nil {
			return nil, err
		}
		segs = append(segs, seg)
	}
	return segs, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	segs, err := splitPath(r.URL.EscapedPath())
	if err != nil {
		writeS3Error(w, http.StatusBadRequest, "InvalidURI", "invalid path encoding", r.URL.Path)
		return
	}
	switch {
	case len(segs) == 0:
		if r.Method == http.MethodGet {
			s.listBuckets(w, r)
			return
		}
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "method not allowed", "/")
	case len(segs) == 1:
		s.handleBucket(w, r, segs[0])
	default:
		s.handleObject(w, r, segs[0], strings.Join(segs[1:], "/"))
	}
}

func (s *Server) handleBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && q.Has("delete"):
		s.deleteObjects(w, r, bucket)
	case r.Method == http.MethodGet && q.Has("location"):
		s.getBucketLocation(w, r, bucket)
	case r.Method == http.MethodGet:
		s.listObjects(w, r, bucket)
	case r.Method == http.MethodPut:
		s.createBucket(w, r, bucket)
	case r.Method == http.MethodHead:
		s.headBucket(w, r, bucket)
	case r.Method == http.MethodDelete:
		s.deleteBucket(w, r, bucket)
	default:
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "method not allowed", "/"+bucket)
	}
}

func (s *Server) handleObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPut && q.Get("uploadId") != "" && q.Get("partNumber") != "":
		s.uploadPart(w, r, bucket, key)
	case r.Method == http.MethodPost && q.Has("uploads"):
		s.initiateMultipartUpload(w, r, bucket, key)
	case r.Method == http.MethodPost && q.Get("uploadId") != "":
		s.completeMultipartUpload(w, r, bucket, key)
	case r.Method == http.MethodDelete && q.Get("uploadId") != "":
		s.abortMultipartUpload(w, r, bucket, key)
	case r.Method == http.MethodPut && r.Header.Get("x-amz-copy-source") != "":
		s.copyObject(w, r, bucket, key)
	case r.Method == http.MethodPut:
		s.putObject(w, r, bucket, key)
	case r.Method == http.MethodGet, r.Method == http.MethodHead:
		s.getObject(w, r, bucket, key)
	case r.Method == http.MethodDelete:
		s.deleteObject(w, r, bucket, key)
	default:
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "method not allowed", "/"+bucket+"/"+key)
	}
}

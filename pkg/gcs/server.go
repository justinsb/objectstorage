// Package gcs implements (a useful subset of) the Google Cloud Storage JSON
// API on top of the store package, sufficient to work with the official
// cloud.google.com/go/storage client pointed at us via STORAGE_EMULATOR_HOST.
package gcs

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/justinsb/objectstorage/pkg/store"
)

// Server is an http.Handler implementing the GCS JSON API.
type Server struct {
	store *store.Store

	mu       sync.Mutex
	sessions map[string]*uploadSession // resumable uploads by upload_id
}

func NewServer(st *store.Store) *Server {
	return &Server{store: st, sessions: make(map[string]*uploadSession)}
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
		writeError(w, http.StatusBadRequest, "invalid", "invalid path encoding")
		return
	}
	switch {
	// /storage/v1/b/...
	case len(segs) >= 3 && segs[0] == "storage" && segs[1] == "v1" && segs[2] == "b":
		s.handleStorageV1(w, r, segs[3:])
	// /upload/storage/v1/b/{bucket}/o
	case len(segs) == 6 && segs[0] == "upload" && segs[1] == "storage" && segs[2] == "v1" &&
		segs[3] == "b" && segs[5] == "o":
		s.handleUpload(w, r, segs[4])
	// /download/storage/v1/b/{bucket}/o/{object}
	case len(segs) >= 7 && segs[0] == "download" && segs[1] == "storage" && segs[2] == "v1" &&
		segs[3] == "b" && segs[5] == "o":
		s.serveMedia(w, r, segs[4], strings.Join(segs[6:], "/"))
	// Direct media path: /{bucket}/{object...} (used by client reads).
	case len(segs) >= 2:
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "invalid", "method not allowed")
			return
		}
		s.serveMedia(w, r, segs[0], strings.Join(segs[1:], "/"))
	default:
		writeError(w, http.StatusNotFound, "notFound", "not found")
	}
}

// handleStorageV1 dispatches /storage/v1/b/... requests. segs is the path
// after the "b" segment.
func (s *Server) handleStorageV1(w http.ResponseWriter, r *http.Request, segs []string) {
	switch {
	case len(segs) == 0:
		switch r.Method {
		case http.MethodGet:
			s.listBuckets(w, r)
		case http.MethodPost:
			s.createBucket(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "invalid", "method not allowed")
		}
	case len(segs) == 1:
		switch r.Method {
		case http.MethodGet:
			s.getBucket(w, r, segs[0])
		case http.MethodDelete:
			s.deleteBucket(w, r, segs[0])
		default:
			writeError(w, http.StatusNotImplemented, "notImplemented", "not implemented")
		}
	case segs[1] == "o" && len(segs) == 2:
		if r.Method == http.MethodGet {
			s.listObjects(w, r, segs[0])
			return
		}
		writeError(w, http.StatusMethodNotAllowed, "invalid", "method not allowed")
	case segs[1] == "o" && len(segs) >= 3:
		bucket, object := segs[0], strings.Join(segs[2:], "/")
		switch r.Method {
		case http.MethodGet:
			if r.URL.Query().Get("alt") == "media" {
				s.serveMedia(w, r, bucket, object)
				return
			}
			s.getObject(w, r, bucket, object)
		case http.MethodDelete:
			s.deleteObject(w, r, bucket, object)
		default:
			writeError(w, http.StatusNotImplemented, "notImplemented",
				"this server does not implement "+r.Method+" on objects (metadata update, compose, rewrite)")
		}
	default:
		writeError(w, http.StatusNotFound, "notFound", "not found")
	}
}

// parseConditions extracts GCS precondition query parameters.
func parseConditions(q url.Values) (store.Conditions, error) {
	var conds store.Conditions
	parse := func(key string, dst **int64) error {
		if v := q.Get(key); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return err
			}
			*dst = &n
		}
		return nil
	}
	if err := parse("ifGenerationMatch", &conds.GenerationMatch); err != nil {
		return conds, err
	}
	if err := parse("ifGenerationNotMatch", &conds.GenerationNotMatch); err != nil {
		return conds, err
	}
	if err := parse("ifMetagenerationMatch", &conds.MetagenerationMatch); err != nil {
		return conds, err
	}
	if err := parse("ifMetagenerationNotMatch", &conds.MetagenerationNotMatch); err != nil {
		return conds, err
	}
	return conds, nil
}

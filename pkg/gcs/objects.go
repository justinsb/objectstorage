package gcs

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/justinsb/objectstorage/pkg/store"
)

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, bucket, object string) {
	info, err := s.store.GetObject(bucket, object)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := checkGenerationParam(r, info.Generation); err != nil {
		writeStoreError(w, err)
		return
	}
	conds, err := parseConditions(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if err := checkReadConditions(conds, info); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toObjectResource(info))
}

func (s *Server) deleteObject(w http.ResponseWriter, r *http.Request, bucket, object string) {
	conds, err := parseConditions(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if err := s.store.DeleteObject(bucket, object, conds); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	q := r.URL.Query()
	req := store.ListRequest{
		Prefix:    q.Get("prefix"),
		Delimiter: q.Get("delimiter"),
		PageToken: q.Get("pageToken"),
	}
	if v := q.Get("maxResults"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid", "invalid maxResults")
			return
		}
		req.MaxResults = n
	}
	result, err := s.store.ListObjects(bucket, req)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	resp := objectsResource{
		Kind:          "storage#objects",
		Prefixes:      result.Prefixes,
		NextPageToken: result.NextPageToken,
	}
	for i := range result.Objects {
		resp.Items = append(resp.Items, toObjectResource(&result.Objects[i]))
	}
	writeJSON(w, http.StatusOK, resp)
}

// checkGenerationParam enforces the ?generation= query parameter on reads.
// Only the live generation is stored, so any other generation is "not found".
func checkGenerationParam(r *http.Request, liveGeneration int64) error {
	v := r.URL.Query().Get("generation")
	if v == "" {
		return nil
	}
	gen, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: invalid generation", store.ErrInvalidName)
	}
	if gen != liveGeneration {
		return store.ErrObjectNotFound
	}
	return nil
}

// checkReadConditions applies preconditions to an already-fetched object.
func checkReadConditions(conds store.Conditions, info *store.ObjectInfo) error {
	if conds.GenerationMatch != nil && info.Generation != *conds.GenerationMatch {
		return store.ErrPreconditionFailed
	}
	if conds.GenerationNotMatch != nil && info.Generation == *conds.GenerationNotMatch {
		return store.ErrPreconditionFailed
	}
	if conds.MetagenerationMatch != nil && info.Metageneration != *conds.MetagenerationMatch {
		return store.ErrPreconditionFailed
	}
	if conds.MetagenerationNotMatch != nil && info.Metageneration == *conds.MetagenerationNotMatch {
		return store.ErrPreconditionFailed
	}
	return nil
}

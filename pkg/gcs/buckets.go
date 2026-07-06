package gcs

import (
	"encoding/json"
	"net/http"
)

func (s *Server) createBucket(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", "invalid bucket resource: "+err.Error())
		return
	}
	info, err := s.store.CreateBucket(req.Name)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toBucketResource(info))
}

func (s *Server) getBucket(w http.ResponseWriter, r *http.Request, name string) {
	info, err := s.store.GetBucket(name)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toBucketResource(info))
}

func (s *Server) listBuckets(w http.ResponseWriter, r *http.Request) {
	infos, err := s.store.ListBuckets()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	resp := bucketsResource{Kind: "storage#buckets"}
	for i := range infos {
		resp.Items = append(resp.Items, toBucketResource(&infos[i]))
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) deleteBucket(w http.ResponseWriter, r *http.Request, name string) {
	if err := s.store.DeleteBucket(name); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

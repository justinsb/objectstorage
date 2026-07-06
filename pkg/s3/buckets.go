package s3

import (
	"net/http"

	"github.com/justinsb/objectstorage/pkg/store"
)

func (s *Server) listBuckets(w http.ResponseWriter, r *http.Request) {
	infos, err := s.store.ListBuckets()
	if err != nil {
		writeStoreError(w, err, "/")
		return
	}
	resp := listAllMyBucketsResult{Owner: defaultOwner}
	for _, info := range infos {
		resp.Buckets = append(resp.Buckets, bucketEntry{
			Name:         info.Name,
			CreationDate: iso8601(info.Created),
		})
	}
	writeXML(w, http.StatusOK, resp)
}

func (s *Server) createBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	if _, err := s.store.CreateBucket(bucket); err != nil {
		writeStoreError(w, err, "/"+bucket)
		return
	}
	w.Header().Set("Location", "/"+bucket)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) headBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	if _, err := s.store.GetBucket(bucket); err != nil {
		// HEAD responses carry no body.
		w.WriteHeader(statusForStoreError(err))
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) deleteBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	if err := s.store.DeleteBucket(bucket); err != nil {
		writeStoreError(w, err, "/"+bucket)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getBucketLocation(w http.ResponseWriter, r *http.Request, bucket string) {
	if _, err := s.store.GetBucket(bucket); err != nil {
		writeStoreError(w, err, "/"+bucket)
		return
	}
	// An empty LocationConstraint means us-east-1.
	writeXML(w, http.StatusOK, locationConstraint{})
}

func statusForStoreError(err error) int {
	switch err {
	case store.ErrBucketNotFound, store.ErrObjectNotFound:
		return http.StatusNotFound
	case store.ErrPreconditionFailed:
		return http.StatusPreconditionFailed
	default:
		return http.StatusInternalServerError
	}
}

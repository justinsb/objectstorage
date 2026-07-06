package s3

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/justinsb/objectstorage/pkg/store"
)

// multipartUpload is an in-progress multipart upload; parts are staged as
// temp files in the bucket's tmp directory until Complete or Abort.
type multipartUpload struct {
	mu          sync.Mutex
	bucket, key string
	contentType string
	metadata    map[string]string
	parts       map[int]*stagedPart
	done        bool
}

type stagedPart struct {
	path string
	size int64
	md5  []byte
}

func (up *multipartUpload) removePartsLocked() {
	for _, p := range up.parts {
		os.Remove(p.path)
	}
	up.parts = nil
}

func (s *Server) initiateMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if _, err := s.store.GetBucket(bucket); err != nil {
		writeStoreError(w, err, "/"+bucket)
		return
	}
	var idBytes [16]byte
	rand.Read(idBytes[:])
	uploadID := hex.EncodeToString(idBytes[:])

	s.mu.Lock()
	s.uploads[uploadID] = &multipartUpload{
		bucket:      bucket,
		key:         key,
		contentType: r.Header.Get("Content-Type"),
		metadata:    metadataFromHeaders(r.Header),
		parts:       map[int]*stagedPart{},
	}
	s.mu.Unlock()

	writeXML(w, http.StatusOK, initiateMultipartUploadResult{
		Bucket: bucket, Key: key, UploadID: uploadID,
	})
}

func (s *Server) lookupUpload(w http.ResponseWriter, r *http.Request, bucket, key string) *multipartUpload {
	uploadID := r.URL.Query().Get("uploadId")
	s.mu.Lock()
	up := s.uploads[uploadID]
	s.mu.Unlock()
	if up == nil || up.bucket != bucket || up.key != key {
		writeS3Error(w, http.StatusNotFound, "NoSuchUpload",
			"The specified upload does not exist.", "/"+bucket+"/"+key)
		return nil
	}
	return up
}

func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, bucket, key string) {
	up := s.lookupUpload(w, r, bucket, key)
	if up == nil {
		return
	}
	partNumber, err := strconv.Atoi(r.URL.Query().Get("partNumber"))
	if err != nil || partNumber < 1 || partNumber > 10000 {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "invalid partNumber", "/"+bucket+"/"+key)
		return
	}

	tmp, err := s.store.TempFile(bucket)
	if err != nil {
		writeStoreError(w, err, "/"+bucket+"/"+key)
		return
	}
	hasher := md5.New()
	size, err := io.Copy(io.MultiWriter(tmp, hasher), requestBody(r))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error(), "/"+bucket+"/"+key)
		return
	}
	part := &stagedPart{path: tmp.Name(), size: size, md5: hasher.Sum(nil)}

	up.mu.Lock()
	if up.done {
		up.mu.Unlock()
		os.Remove(part.path)
		writeS3Error(w, http.StatusNotFound, "NoSuchUpload", "upload already completed", "/"+bucket+"/"+key)
		return
	}
	if prev := up.parts[partNumber]; prev != nil {
		os.Remove(prev.path)
	}
	up.parts[partNumber] = part
	up.mu.Unlock()

	w.Header().Set("ETag", `"`+hex.EncodeToString(part.md5)+`"`)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) completeMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	up := s.lookupUpload(w, r, bucket, key)
	if up == nil {
		return
	}
	var req completeMultipartUpload
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", err.Error(), "/"+bucket+"/"+key)
		return
	}
	if len(req.Parts) == 0 {
		writeS3Error(w, http.StatusBadRequest, "InvalidRequest", "no parts specified", "/"+bucket+"/"+key)
		return
	}
	if !sort.SliceIsSorted(req.Parts, func(i, j int) bool {
		return req.Parts[i].PartNumber < req.Parts[j].PartNumber
	}) {
		writeS3Error(w, http.StatusBadRequest, "InvalidPartOrder",
			"The list of parts was not in ascending order.", "/"+bucket+"/"+key)
		return
	}

	up.mu.Lock()
	defer up.mu.Unlock()
	if up.done {
		writeS3Error(w, http.StatusNotFound, "NoSuchUpload", "upload already completed", "/"+bucket+"/"+key)
		return
	}
	for _, reqPart := range req.Parts {
		part := up.parts[reqPart.PartNumber]
		if part == nil {
			writeS3Error(w, http.StatusBadRequest, "InvalidPart",
				fmt.Sprintf("part %d was not uploaded", reqPart.PartNumber), "/"+bucket+"/"+key)
			return
		}
		if want := strings.Trim(reqPart.ETag, `"`); want != "" && want != hex.EncodeToString(part.md5) {
			writeS3Error(w, http.StatusBadRequest, "InvalidPart",
				fmt.Sprintf("part %d etag mismatch", reqPart.PartNumber), "/"+bucket+"/"+key)
			return
		}
	}

	bw, err := s.store.NewBlobWriter(bucket)
	if err != nil {
		writeStoreError(w, err, "/"+bucket+"/"+key)
		return
	}
	for _, reqPart := range req.Parts {
		part := up.parts[reqPart.PartNumber]
		f, err := os.Open(part.path)
		if err == nil {
			_, err = io.Copy(bw, f)
			f.Close()
		}
		if err != nil {
			bw.Abort()
			writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error(), "/"+bucket+"/"+key)
			return
		}
	}
	info, err := bw.Commit(key, store.PutOptions{
		ContentType: up.contentType,
		Metadata:    up.metadata,
	})
	if err != nil {
		writeStoreError(w, err, "/"+bucket+"/"+key)
		return
	}

	up.done = true
	up.removePartsLocked()
	s.mu.Lock()
	delete(s.uploads, r.URL.Query().Get("uploadId"))
	s.mu.Unlock()

	// Note: real S3 multipart ETags are "<md5-of-part-md5s>-<n>"; we return
	// the content MD5 instead, which is friendlier to integrity checks.
	writeXML(w, http.StatusOK, completeMultipartUploadResult{
		Location: "/" + bucket + "/" + key,
		Bucket:   bucket,
		Key:      key,
		ETag:     etagFor(info),
	})
}

func (s *Server) abortMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	up := s.lookupUpload(w, r, bucket, key)
	if up == nil {
		return
	}
	up.mu.Lock()
	up.done = true
	up.removePartsLocked()
	up.mu.Unlock()
	s.mu.Lock()
	delete(s.uploads, r.URL.Query().Get("uploadId"))
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

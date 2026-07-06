package gcs

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"regexp"
	"strconv"
	"sync"

	"github.com/justinsb/objectstorage/pkg/store"
)

// objectMetadata is the subset of the object resource we accept on upload.
type objectMetadata struct {
	Name        string            `json:"name"`
	ContentType string            `json:"contentType"`
	Metadata    map[string]string `json:"metadata"`
	MD5Hash     string            `json:"md5Hash"`
	CRC32C      string            `json:"crc32c"`
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request, bucket string) {
	q := r.URL.Query()
	if uploadID := q.Get("upload_id"); uploadID != "" {
		s.handleResumableChunk(w, r, uploadID)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid", "method not allowed")
		return
	}
	conds, err := parseConditions(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	switch q.Get("uploadType") {
	case "media":
		meta := objectMetadata{Name: q.Get("name"), ContentType: r.Header.Get("Content-Type")}
		s.putObject(w, r.Body, bucket, meta, conds)
	case "multipart":
		s.handleMultipartUpload(w, r, bucket, conds)
	case "resumable":
		s.startResumableUpload(w, r, bucket, conds)
	default:
		writeError(w, http.StatusBadRequest, "invalid", "unsupported uploadType")
	}
}

// putObject streams media into the store and writes the object resource.
func (s *Server) putObject(w http.ResponseWriter, media io.Reader, bucket string, meta objectMetadata, conds store.Conditions) {
	bw, err := s.store.NewBlobWriter(bucket)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if _, err := io.Copy(bw, media); err != nil {
		bw.Abort()
		writeError(w, http.StatusBadRequest, "invalid", "reading upload body: "+err.Error())
		return
	}
	s.commitUpload(w, bw, bucket, meta, conds)
}

// commitUpload verifies client-supplied checksums, commits the blob, and
// writes the object resource response.
func (s *Server) commitUpload(w http.ResponseWriter, bw *store.BlobWriter, bucket string, meta objectMetadata, conds store.Conditions) {
	if meta.Name == "" {
		bw.Abort()
		writeError(w, http.StatusBadRequest, "required", "object name is required")
		return
	}
	_, md5Sum, crc32c := bw.Checksums()
	if meta.MD5Hash != "" {
		want, err := base64.StdEncoding.DecodeString(meta.MD5Hash)
		if err != nil || !bytes.Equal(want, md5Sum) {
			bw.Abort()
			writeError(w, http.StatusBadRequest, "invalid", "md5Hash does not match uploaded content")
			return
		}
	}
	if meta.CRC32C != "" {
		want, err := base64.StdEncoding.DecodeString(meta.CRC32C)
		if err != nil || len(want) != 4 || binary.BigEndian.Uint32(want) != crc32c {
			bw.Abort()
			writeError(w, http.StatusBadRequest, "invalid", "crc32c does not match uploaded content")
			return
		}
	}
	info, err := bw.Commit(meta.Name, store.PutOptions{
		ContentType: meta.ContentType,
		Metadata:    meta.Metadata,
		Conditions:  conds,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toObjectResource(info))
}

// handleMultipartUpload handles uploadType=multipart: a multipart/related
// body with a JSON metadata part followed by a media part.
func (s *Server) handleMultipartUpload(w http.ResponseWriter, r *http.Request, bucket string, conds store.Conditions) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || params["boundary"] == "" {
		writeError(w, http.StatusBadRequest, "invalid", "expected multipart/related body")
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])

	metaPart, err := mr.NextPart()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", "missing metadata part")
		return
	}
	var meta objectMetadata
	if err := json.NewDecoder(metaPart).Decode(&meta); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", "invalid metadata part: "+err.Error())
		return
	}
	if meta.Name == "" {
		meta.Name = r.URL.Query().Get("name")
	}

	mediaPart, err := mr.NextPart()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", "missing media part")
		return
	}
	if meta.ContentType == "" {
		meta.ContentType = mediaPart.Header.Get("Content-Type")
	}
	s.putObject(w, mediaPart, bucket, meta, conds)
}

// uploadSession is an in-progress resumable upload.
type uploadSession struct {
	mu     sync.Mutex
	bucket string
	meta   objectMetadata
	conds  store.Conditions
	bw     *store.BlobWriter
	offset int64
	done   bool
}

func (s *Server) startResumableUpload(w http.ResponseWriter, r *http.Request, bucket string, conds store.Conditions) {
	var meta objectMetadata
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&meta); err != nil && err != io.EOF {
			writeError(w, http.StatusBadRequest, "invalid", "invalid metadata: "+err.Error())
			return
		}
	}
	if meta.Name == "" {
		meta.Name = r.URL.Query().Get("name")
	}
	bw, err := s.store.NewBlobWriter(bucket)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	var idBytes [16]byte
	rand.Read(idBytes[:])
	uploadID := hex.EncodeToString(idBytes[:])

	s.mu.Lock()
	s.sessions[uploadID] = &uploadSession{bucket: bucket, meta: meta, conds: conds, bw: bw}
	s.mu.Unlock()

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	w.Header().Set("Location", fmt.Sprintf("%s://%s%s?uploadType=resumable&upload_id=%s",
		scheme, r.Host, r.URL.EscapedPath(), uploadID))
	w.Header().Set("X-GUploader-UploadID", uploadID)
	w.WriteHeader(http.StatusOK)
}

var (
	reContentRangeChunk = regexp.MustCompile(`^bytes (\d+)-(\d+)/(\d+|\*)$`)
	reContentRangeProbe = regexp.MustCompile(`^bytes \*/(\d+|\*)$`)
)

func (s *Server) handleResumableChunk(w http.ResponseWriter, r *http.Request, uploadID string) {
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid", "method not allowed")
		return
	}
	s.mu.Lock()
	sess := s.sessions[uploadID]
	s.mu.Unlock()
	if sess == nil {
		writeError(w, http.StatusNotFound, "notFound", "no such upload session")
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.done {
		writeError(w, http.StatusGone, "invalid", "upload session already finalized")
		return
	}

	contentRange := r.Header.Get("Content-Range")
	total := int64(-1) // -1: unknown

	if m := reContentRangeProbe.FindStringSubmatch(contentRange); m != nil {
		if m[1] != "*" {
			total, _ = strconv.ParseInt(m[1], 10, 64)
		}
	} else if m := reContentRangeChunk.FindStringSubmatch(contentRange); m != nil {
		start, _ := strconv.ParseInt(m[1], 10, 64)
		end, _ := strconv.ParseInt(m[2], 10, 64)
		if m[3] != "*" {
			total, _ = strconv.ParseInt(m[3], 10, 64)
		}
		if start != sess.offset {
			// Out-of-order chunk; report current progress.
			s.writeResumeIncomplete(w, r, sess)
			return
		}
		n, err := io.Copy(sess.bw, r.Body)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internalError", "writing chunk: "+err.Error())
			return
		}
		if n != end-start+1 {
			writeError(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("chunk body length %d does not match Content-Range %s", n, contentRange))
			return
		}
		sess.offset += n
	} else if contentRange == "" {
		// Single-request upload of the whole content.
		n, err := io.Copy(sess.bw, r.Body)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internalError", "writing chunk: "+err.Error())
			return
		}
		sess.offset += n
		total = sess.offset
	} else {
		writeError(w, http.StatusBadRequest, "invalid", "invalid Content-Range: "+contentRange)
		return
	}

	if total >= 0 && sess.offset == total {
		sess.done = true
		s.mu.Lock()
		delete(s.sessions, uploadID)
		s.mu.Unlock()
		s.commitUpload(w, sess.bw, sess.bucket, sess.meta, sess.conds)
		return
	}
	s.writeResumeIncomplete(w, r, sess)
}

// writeResumeIncomplete reports partial progress. Historically this is a
// "308 Resume Incomplete" response, but Go's http.Client treats 308 as a
// redirect, so Google's clients send "X-GUploader-No-308: yes" and expect a
// 200 with an X-Http-Status-Code-Override header instead.
func (s *Server) writeResumeIncomplete(w http.ResponseWriter, r *http.Request, sess *uploadSession) {
	if sess.offset > 0 {
		w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", sess.offset-1))
	}
	if r.Header.Get("X-GUploader-No-308") == "yes" {
		w.Header().Set("X-Http-Status-Code-Override", "308")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(308) // Resume Incomplete
}

package gcs

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
)

// serveMedia serves object content, with Range support, on both the direct
// download path (/{bucket}/{object}) and the JSON API alt=media paths.
func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request, bucket, object string) {
	info, f, err := s.store.OpenObject(bucket, object)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer f.Close()

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

	contentType := info.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	// The official clients parse these headers and verify the crc32c
	// checksum of full-object reads against X-Goog-Hash.
	h.Set("X-Goog-Generation", strconv.FormatInt(info.Generation, 10))
	h.Set("X-Goog-Metageneration", strconv.FormatInt(info.Metageneration, 10))
	h.Set("X-Goog-Stored-Content-Length", strconv.FormatInt(info.Size, 10))
	h.Set("X-Goog-Stored-Content-Encoding", "identity")
	h.Set("X-Goog-Storage-Class", "STANDARD")
	h.Set("X-Goog-Hash", fmt.Sprintf("crc32c=%s,md5=%s",
		encodeCRC32C(info.CRC32C), base64.StdEncoding.EncodeToString(info.MD5)))
	h.Set("ETag", fmt.Sprintf("g%dm%d", info.Generation, info.Metageneration))

	// ServeContent handles Range requests, HEAD, and status codes for us.
	http.ServeContent(w, r, "", info.Updated, f)
}

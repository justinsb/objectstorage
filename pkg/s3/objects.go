package s3

import (
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/justinsb/objectstorage/pkg/store"
)

func etagFor(info *store.ObjectInfo) string {
	return `"` + hex.EncodeToString(info.MD5) + `"`
}

// metadataFromHeaders collects x-amz-meta-* headers into a metadata map.
func metadataFromHeaders(h http.Header) map[string]string {
	var md map[string]string
	for name, values := range h {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-meta-") && len(values) > 0 {
			if md == nil {
				md = map[string]string{}
			}
			md[strings.TrimPrefix(lower, "x-amz-meta-")] = values[0]
		}
	}
	return md
}

func (s *Server) putObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	opts := store.PutOptions{
		ContentType: r.Header.Get("Content-Type"),
		Metadata:    metadataFromHeaders(r.Header),
	}
	// If-None-Match: * gives S3's conditional create.
	if r.Header.Get("If-None-Match") == "*" {
		zero := int64(0)
		opts.Conditions.GenerationMatch = &zero
	}
	info, err := s.store.PutObject(bucket, key, requestBody(r), opts)
	if err != nil {
		writeStoreError(w, err, "/"+bucket+"/"+key)
		return
	}
	w.Header().Set("ETag", etagFor(info))
	w.WriteHeader(http.StatusOK)
}

// getObject serves both GET and HEAD.
func (s *Server) getObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	info, f, err := s.store.OpenObject(bucket, key)
	if err != nil {
		if r.Method == http.MethodHead {
			w.WriteHeader(statusForStoreError(err))
			return
		}
		writeStoreError(w, err, "/"+bucket+"/"+key)
		return
	}
	defer f.Close()

	contentType := info.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("ETag", etagFor(info))
	for k, v := range info.Metadata {
		h.Set("x-amz-meta-"+k, v)
	}
	// ServeContent handles Range, HEAD, and If-* conditional headers
	// (using the ETag we just set).
	http.ServeContent(w, r, "", info.Updated, f)
}

func (s *Server) deleteObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	err := s.store.DeleteObject(bucket, key, store.Conditions{})
	// S3 DeleteObject is idempotent: deleting a missing key succeeds.
	if err != nil && err != store.ErrObjectNotFound {
		writeStoreError(w, err, "/"+bucket+"/"+key)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	var req deleteRequest
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", err.Error(), "/"+bucket)
		return
	}
	var resp deleteResult
	for _, obj := range req.Objects {
		err := s.store.DeleteObject(bucket, obj.Key, store.Conditions{})
		if err != nil && err != store.ErrObjectNotFound {
			resp.Errors = append(resp.Errors, deleteError{
				Key: obj.Key, Code: "InternalError", Message: err.Error(),
			})
			continue
		}
		if !req.Quiet {
			resp.Deleted = append(resp.Deleted, deletedObject{Key: obj.Key})
		}
	}
	writeXML(w, http.StatusOK, resp)
}

func (s *Server) copyObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	srcBucket, srcKey, err := parseCopySource(r.Header.Get("x-amz-copy-source"))
	if err != nil {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", err.Error(), "/"+bucket+"/"+key)
		return
	}
	srcInfo, f, err := s.store.OpenObject(srcBucket, srcKey)
	if err != nil {
		writeStoreError(w, err, "/"+srcBucket+"/"+srcKey)
		return
	}
	defer f.Close()

	opts := store.PutOptions{ContentType: srcInfo.ContentType, Metadata: srcInfo.Metadata}
	if strings.EqualFold(r.Header.Get("x-amz-metadata-directive"), "REPLACE") {
		opts.ContentType = r.Header.Get("Content-Type")
		opts.Metadata = metadataFromHeaders(r.Header)
	}
	info, err := s.store.PutObject(bucket, key, f, opts)
	if err != nil {
		writeStoreError(w, err, "/"+bucket+"/"+key)
		return
	}
	writeXML(w, http.StatusOK, copyObjectResult{
		ETag:         etagFor(info),
		LastModified: iso8601(info.Updated),
	})
}

// prefixSuccessor returns the smallest string greater than every string with
// the given prefix, or "" if there is none.
func prefixSuccessor(prefix string) string {
	for i := len(prefix) - 1; i >= 0; i-- {
		if prefix[i] != 0xff {
			return prefix[:i] + string(prefix[i]+1)
		}
	}
	return ""
}

// parseCopySource parses "x-amz-copy-source: [/]bucket/key" (URL-encoded).
func parseCopySource(src string) (bucket, key string, err error) {
	src = strings.TrimPrefix(src, "/")
	unescaped, err := url.PathUnescape(src)
	if err != nil {
		return "", "", fmt.Errorf("invalid x-amz-copy-source encoding: %q", src)
	}
	bucket, key, ok := strings.Cut(unescaped, "/")
	if !ok || bucket == "" || key == "" {
		return "", "", fmt.Errorf("invalid x-amz-copy-source: %q", src)
	}
	return bucket, key, nil
}

func (s *Server) listObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	q := r.URL.Query()
	isV2 := q.Get("list-type") == "2"
	encodingType := q.Get("encoding-type")

	req := store.ListRequest{
		Prefix:    q.Get("prefix"),
		Delimiter: q.Get("delimiter"),
	}
	maxKeys := 1000
	if v := q.Get("max-keys"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "invalid max-keys", "/"+bucket)
			return
		}
		maxKeys = n
	}
	req.MaxResults = maxKeys

	// Map S3 cursors onto the store's inclusive PageToken.
	var contToken, marker, startAfter string
	if isV2 {
		contToken = q.Get("continuation-token")
		startAfter = q.Get("start-after")
		if contToken != "" {
			req.PageToken = contToken
		} else if startAfter != "" {
			req.PageToken = startAfter + "\x00" // start-after is exclusive
		}
	} else {
		marker = q.Get("marker")
		if marker != "" {
			if req.Delimiter != "" && strings.HasSuffix(marker, req.Delimiter) {
				// The marker is a common prefix (as set in NextMarker);
				// resume after the entire group, not inside it.
				req.PageToken = prefixSuccessor(marker)
			} else {
				req.PageToken = marker + "\x00" // marker is exclusive
			}
		}
	}

	result, err := s.store.ListObjects(bucket, req)
	if err != nil {
		writeStoreError(w, err, "/"+bucket)
		return
	}

	encode := func(v string) string {
		if encodingType == "url" {
			return url.QueryEscape(v)
		}
		return v
	}
	var items []contents
	for i := range result.Objects {
		o := &result.Objects[i]
		items = append(items, contents{
			Key:          encode(o.Name),
			LastModified: iso8601(o.Updated),
			ETag:         etagFor(o),
			Size:         o.Size,
			StorageClass: "STANDARD",
		})
	}
	var prefixes []commonPrefix
	for _, p := range result.Prefixes {
		prefixes = append(prefixes, commonPrefix{Prefix: encode(p)})
	}

	if isV2 {
		writeXML(w, http.StatusOK, listBucketResultV2{
			Name:                  bucket,
			Prefix:                encode(req.Prefix),
			Delimiter:             encode(req.Delimiter),
			MaxKeys:               maxKeys,
			EncodingType:          encodingType,
			KeyCount:              len(items) + len(prefixes),
			IsTruncated:           result.NextPageToken != "",
			ContinuationToken:     contToken,
			NextContinuationToken: result.NextPageToken,
			StartAfter:            encode(startAfter),
			Contents:              items,
			CommonPrefixes:        prefixes,
		})
		return
	}

	v1 := listBucketResultV1{
		Name:           bucket,
		Prefix:         encode(req.Prefix),
		Delimiter:      encode(req.Delimiter),
		Marker:         encode(marker),
		MaxKeys:        maxKeys,
		EncodingType:   encodingType,
		IsTruncated:    result.NextPageToken != "",
		Contents:       items,
		CommonPrefixes: prefixes,
	}
	if v1.IsTruncated {
		// NextMarker is the last name returned in this page; the store's
		// NextPageToken is the *next* name, which marker semantics
		// (exclusive) cannot represent directly.
		last := ""
		if n := len(result.Objects); n > 0 {
			last = result.Objects[n-1].Name
		}
		if n := len(result.Prefixes); n > 0 && result.Prefixes[n-1] > last {
			last = result.Prefixes[n-1]
		}
		v1.NextMarker = encode(last)
	}
	writeXML(w, http.StatusOK, v1)
}

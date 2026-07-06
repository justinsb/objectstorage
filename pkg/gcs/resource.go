package gcs

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/justinsb/objectstorage/pkg/store"
)

// Wire types for the GCS JSON API. Field names and the ",string" encoding of
// int64 values must match google-api-go-client's storage/v1 types.

type bucketResource struct {
	Kind           string `json:"kind"`
	ID             string `json:"id"`
	Name           string `json:"name"`
	Location       string `json:"location"`
	StorageClass   string `json:"storageClass"`
	Metageneration int64  `json:"metageneration,string"`
	TimeCreated    string `json:"timeCreated"`
	Updated        string `json:"updated"`
	Etag           string `json:"etag"`
}

type bucketsResource struct {
	Kind  string           `json:"kind"`
	Items []bucketResource `json:"items,omitempty"`
}

type objectResource struct {
	Kind           string            `json:"kind"`
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Bucket         string            `json:"bucket"`
	Generation     int64             `json:"generation,string"`
	Metageneration int64             `json:"metageneration,string"`
	ContentType    string            `json:"contentType,omitempty"`
	StorageClass   string            `json:"storageClass"`
	Size           uint64            `json:"size,string"`
	MD5Hash        string            `json:"md5Hash"`
	CRC32C         string            `json:"crc32c"`
	Etag           string            `json:"etag"`
	TimeCreated    string            `json:"timeCreated"`
	Updated        string            `json:"updated"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

type objectsResource struct {
	Kind          string           `json:"kind"`
	Items         []objectResource `json:"items,omitempty"`
	Prefixes      []string         `json:"prefixes,omitempty"`
	NextPageToken string           `json:"nextPageToken,omitempty"`
}

func rfc3339(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.999999Z07:00")
}

func encodeCRC32C(v uint32) string {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], v)
	return base64.StdEncoding.EncodeToString(buf[:])
}

func toBucketResource(info *store.BucketInfo) bucketResource {
	return bucketResource{
		Kind:           "storage#bucket",
		ID:             info.Name,
		Name:           info.Name,
		Location:       "US",
		StorageClass:   "STANDARD",
		Metageneration: info.Metageneration,
		TimeCreated:    rfc3339(info.Created),
		Updated:        rfc3339(info.Created),
		Etag:           fmt.Sprintf("m%d", info.Metageneration),
	}
}

func toObjectResource(info *store.ObjectInfo) objectResource {
	return objectResource{
		Kind:           "storage#object",
		ID:             fmt.Sprintf("%s/%s/%d", info.Bucket, info.Name, info.Generation),
		Name:           info.Name,
		Bucket:         info.Bucket,
		Generation:     info.Generation,
		Metageneration: info.Metageneration,
		ContentType:    info.ContentType,
		StorageClass:   "STANDARD",
		Size:           uint64(info.Size),
		MD5Hash:        base64.StdEncoding.EncodeToString(info.MD5),
		CRC32C:         encodeCRC32C(info.CRC32C),
		Etag:           fmt.Sprintf("g%dm%d", info.Generation, info.Metageneration),
		TimeCreated:    rfc3339(info.Created),
		Updated:        rfc3339(info.Updated),
		Metadata:       info.Metadata,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError writes a GCS JSON API error envelope.
func writeError(w http.ResponseWriter, code int, reason, message string) {
	type errorItem struct {
		Domain  string `json:"domain"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	envelope := map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
			"errors":  []errorItem{{Domain: "global", Reason: reason, Message: message}},
		},
	}
	writeJSON(w, code, envelope)
}

// writeStoreError maps store errors onto GCS API error responses.
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrBucketNotFound):
		writeError(w, http.StatusNotFound, "notFound", "The specified bucket does not exist.")
	case errors.Is(err, store.ErrObjectNotFound):
		writeError(w, http.StatusNotFound, "notFound", "No such object")
	case errors.Is(err, store.ErrBucketExists):
		writeError(w, http.StatusConflict, "conflict", "The requested bucket name is not available.")
	case errors.Is(err, store.ErrBucketNotEmpty):
		writeError(w, http.StatusConflict, "conflict", "The bucket you tried to delete is not empty.")
	case errors.Is(err, store.ErrPreconditionFailed):
		writeError(w, http.StatusPreconditionFailed, "conditionNotMet",
			"At least one of the pre-conditions you specified did not hold.")
	case errors.Is(err, store.ErrInvalidName):
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internalError", err.Error())
	}
}

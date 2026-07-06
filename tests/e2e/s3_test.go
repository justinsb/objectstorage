package e2e

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

func (h *Harness) mustCreateS3Bucket(ctx context.Context, name string) {
	h.T.Helper()
	if _, err := h.S3Client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(name)}); err != nil {
		h.T.Fatalf("creating bucket %q: %v", name, err)
	}
}

func isS3ErrorCode(err error, code string) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == code
}

func TestS3ObjectRoundTrip(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	h.mustCreateS3Bucket(ctx, "s3bucket")

	content := []byte("hello from the S3 protocol")
	put, err := h.S3Client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:      aws.String("s3bucket"),
		Key:         aws.String("dir/hello.txt"),
		Body:        bytes.NewReader(content),
		ContentType: aws.String("text/plain"),
		Metadata:    map[string]string{"origin": "s3-e2e"},
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	wantETag := fmt.Sprintf("%q", hex.EncodeToString(md5sum(content)))
	if aws.ToString(put.ETag) != wantETag {
		t.Errorf("put etag = %s, want %s", aws.ToString(put.ETag), wantETag)
	}

	head, err := h.S3Client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String("s3bucket"), Key: aws.String("dir/hello.txt"),
	})
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if aws.ToInt64(head.ContentLength) != int64(len(content)) {
		t.Errorf("head size = %d, want %d", aws.ToInt64(head.ContentLength), len(content))
	}
	if aws.ToString(head.ContentType) != "text/plain" {
		t.Errorf("head contentType = %q, want text/plain", aws.ToString(head.ContentType))
	}
	if head.Metadata["origin"] != "s3-e2e" {
		t.Errorf("head metadata = %v, want origin=s3-e2e", head.Metadata)
	}

	get, err := h.S3Client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String("s3bucket"), Key: aws.String("dir/hello.txt"),
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, err := io.ReadAll(get.Body)
	get.Body.Close()
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("get = %q, want %q", got, content)
	}
	if aws.ToString(get.ETag) != wantETag {
		t.Errorf("get etag = %s, want %s", aws.ToString(get.ETag), wantETag)
	}

	if _, err := h.S3Client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String("s3bucket"), Key: aws.String("dir/hello.txt"),
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = h.S3Client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String("s3bucket"), Key: aws.String("dir/hello.txt"),
	})
	var noSuchKey *types.NoSuchKey
	if !errors.As(err, &noSuchKey) {
		t.Errorf("get after delete: got %v, want NoSuchKey", err)
	}

	// Deleting a missing key is a success in S3.
	if _, err := h.S3Client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String("s3bucket"), Key: aws.String("never-existed"),
	}); err != nil {
		t.Errorf("delete of missing key: %v, want success", err)
	}
}

func md5sum(b []byte) []byte {
	sum := md5.Sum(b)
	return sum[:]
}

func TestS3RangeGet(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	h.mustCreateS3Bucket(ctx, "s3bucket")

	content := []byte("hello, world")
	if _, err := h.S3Client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String("s3bucket"), Key: aws.String("r.txt"), Body: bytes.NewReader(content),
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	get, err := h.S3Client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String("s3bucket"), Key: aws.String("r.txt"),
		Range: aws.String("bytes=7-11"),
	})
	if err != nil {
		t.Fatalf("range get: %v", err)
	}
	got, _ := io.ReadAll(get.Body)
	get.Body.Close()
	if string(got) != "world" {
		t.Errorf("range get = %q, want %q", got, "world")
	}
}

func TestS3ListObjectsV2(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	h.mustCreateS3Bucket(ctx, "s3bucket")

	for _, key := range []string{"a/1.txt", "a/2.txt", "b/3.txt", "top.txt"} {
		if _, err := h.S3Client.PutObject(ctx, &awss3.PutObjectInput{
			Bucket: aws.String("s3bucket"), Key: aws.String(key), Body: bytes.NewReader([]byte(key)),
		}); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}

	out, err := h.S3Client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
		Bucket: aws.String("s3bucket"), Delimiter: aws.String("/"),
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var keys, prefixes []string
	for _, o := range out.Contents {
		keys = append(keys, aws.ToString(o.Key))
	}
	for _, p := range out.CommonPrefixes {
		prefixes = append(prefixes, aws.ToString(p.Prefix))
	}
	if !equalStrings(keys, []string{"top.txt"}) || !equalStrings(prefixes, []string{"a/", "b/"}) {
		t.Errorf("delimiter list = %v / %v, want [top.txt] / [a/ b/]", keys, prefixes)
	}

	// Paginate with MaxKeys=3 over all objects.
	var all []string
	paginator := awss3.NewListObjectsV2Paginator(h.S3Client, &awss3.ListObjectsV2Input{
		Bucket: aws.String("s3bucket"), MaxKeys: aws.Int32(3),
	})
	pages := 0
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			t.Fatalf("paginate: %v", err)
		}
		pages++
		for _, o := range page.Contents {
			all = append(all, aws.ToString(o.Key))
		}
	}
	if !equalStrings(all, []string{"a/1.txt", "a/2.txt", "b/3.txt", "top.txt"}) || pages < 2 {
		t.Errorf("paginated = %v over %d pages, want all 4 keys over >=2 pages", all, pages)
	}
}

func TestS3MultipartUpload(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	h.mustCreateS3Bucket(ctx, "s3bucket")

	content := make([]byte, 12<<20) // 12 MiB: forces multipart at 5 MiB parts
	for i := range content {
		content[i] = byte(i * 13)
	}

	uploader := manager.NewUploader(h.S3Client, func(u *manager.Uploader) {
		u.PartSize = 5 << 20
	})
	if _, err := uploader.Upload(ctx, &awss3.PutObjectInput{
		Bucket:      aws.String("s3bucket"),
		Key:         aws.String("big.bin"),
		Body:        bytes.NewReader(content),
		ContentType: aws.String("application/octet-stream"),
	}); err != nil {
		t.Fatalf("multipart upload: %v", err)
	}

	get, err := h.S3Client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String("s3bucket"), Key: aws.String("big.bin"),
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, err := io.ReadAll(get.Body)
	get.Body.Close()
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("round-tripped content differs (len %d vs %d)", len(got), len(content))
	}
}

func TestS3CopyObject(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	h.mustCreateS3Bucket(ctx, "src")
	h.mustCreateS3Bucket(ctx, "dst")

	content := []byte("copy me")
	if _, err := h.S3Client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String("src"), Key: aws.String("orig.txt"), Body: bytes.NewReader(content),
		ContentType: aws.String("text/plain"), Metadata: map[string]string{"k": "v"},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := h.S3Client.CopyObject(ctx, &awss3.CopyObjectInput{
		Bucket:     aws.String("dst"),
		Key:        aws.String("copied.txt"),
		CopySource: aws.String("src/orig.txt"),
	}); err != nil {
		t.Fatalf("copy: %v", err)
	}
	head, err := h.S3Client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String("dst"), Key: aws.String("copied.txt"),
	})
	if err != nil {
		t.Fatalf("head copy: %v", err)
	}
	if aws.ToString(head.ContentType) != "text/plain" || head.Metadata["k"] != "v" {
		t.Errorf("copy did not preserve contentType/metadata: %q %v",
			aws.ToString(head.ContentType), head.Metadata)
	}
	get, err := h.S3Client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String("dst"), Key: aws.String("copied.txt"),
	})
	if err != nil {
		t.Fatalf("get copy: %v", err)
	}
	got, _ := io.ReadAll(get.Body)
	get.Body.Close()
	if !bytes.Equal(got, content) {
		t.Errorf("copied content = %q, want %q", got, content)
	}
}

func TestS3DeleteObjects(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	h.mustCreateS3Bucket(ctx, "s3bucket")

	var ids []types.ObjectIdentifier
	for _, key := range []string{"d/1", "d/2", "d/3"} {
		if _, err := h.S3Client.PutObject(ctx, &awss3.PutObjectInput{
			Bucket: aws.String("s3bucket"), Key: aws.String(key), Body: bytes.NewReader([]byte(key)),
		}); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
		ids = append(ids, types.ObjectIdentifier{Key: aws.String(key)})
	}
	out, err := h.S3Client.DeleteObjects(ctx, &awss3.DeleteObjectsInput{
		Bucket: aws.String("s3bucket"),
		Delete: &types.Delete{Objects: ids},
	})
	if err != nil {
		t.Fatalf("delete objects: %v", err)
	}
	if len(out.Deleted) != 3 || len(out.Errors) != 0 {
		t.Errorf("deleted %d with %d errors, want 3/0", len(out.Deleted), len(out.Errors))
	}
	list, err := h.S3Client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: aws.String("s3bucket")})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Contents) != 0 {
		t.Errorf("bucket not empty after batch delete: %d keys", len(list.Contents))
	}
}

func TestS3Buckets(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	h.mustCreateS3Bucket(ctx, "alpha")
	h.mustCreateS3Bucket(ctx, "beta")

	if _, err := h.S3Client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String("alpha")}); !isS3ErrorCode(err, "BucketAlreadyOwnedByYou") {
		t.Errorf("duplicate create: got %v, want BucketAlreadyOwnedByYou", err)
	}

	if _, err := h.S3Client.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String("alpha")}); err != nil {
		t.Errorf("head bucket: %v", err)
	}
	if _, err := h.S3Client.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String("nope")}); err == nil {
		t.Errorf("head of missing bucket succeeded, want error")
	}

	out, err := h.S3Client.ListBuckets(ctx, &awss3.ListBucketsInput{})
	if err != nil {
		t.Fatalf("list buckets: %v", err)
	}
	var names []string
	for _, b := range out.Buckets {
		names = append(names, aws.ToString(b.Name))
	}
	if !equalStrings(names, []string{"alpha", "beta"}) {
		t.Errorf("buckets = %v, want [alpha beta]", names)
	}

	// Non-empty bucket cannot be deleted.
	if _, err := h.S3Client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String("beta"), Key: aws.String("x"), Body: bytes.NewReader([]byte("x")),
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := h.S3Client.DeleteBucket(ctx, &awss3.DeleteBucketInput{Bucket: aws.String("beta")}); !isS3ErrorCode(err, "BucketNotEmpty") {
		t.Errorf("delete non-empty: got %v, want BucketNotEmpty", err)
	}
	if _, err := h.S3Client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String("beta"), Key: aws.String("x")}); err != nil {
		t.Fatalf("delete object: %v", err)
	}
	if _, err := h.S3Client.DeleteBucket(ctx, &awss3.DeleteBucketInput{Bucket: aws.String("beta")}); err != nil {
		t.Errorf("delete empty bucket: %v", err)
	}
}

// TestS3GCSInterop verifies that both protocol frontends share one store:
// objects written through one are readable through the other, and identical
// content is deduplicated across protocols.
func TestS3GCSInterop(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	bkt := h.MustCreateBucket(ctx, "shared") // created via GCS

	content := []byte("one store, two protocols")

	// Write via GCS, read via S3.
	w := bkt.Object("via-gcs.txt").NewWriter(ctx)
	w.ContentType = "text/plain"
	w.Write(content)
	if err := w.Close(); err != nil {
		t.Fatalf("gcs write: %v", err)
	}
	get, err := h.S3Client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String("shared"), Key: aws.String("via-gcs.txt"),
	})
	if err != nil {
		t.Fatalf("s3 read of gcs object: %v", err)
	}
	got, _ := io.ReadAll(get.Body)
	get.Body.Close()
	if !bytes.Equal(got, content) {
		t.Errorf("s3 read = %q, want %q", got, content)
	}

	// Write via S3, read via GCS.
	if _, err := h.S3Client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String("shared"), Key: aws.String("via-s3.txt"), Body: bytes.NewReader(content),
	}); err != nil {
		t.Fatalf("s3 write: %v", err)
	}
	r, err := bkt.Object("via-s3.txt").NewReader(ctx)
	if err != nil {
		t.Fatalf("gcs read of s3 object: %v", err)
	}
	got, err = io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatalf("gcs read: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("gcs read = %q, want %q", got, content)
	}

	// Identical content written via both protocols shares one CAS blob.
	if n := h.CountBlobFiles("shared"); n != 1 {
		t.Errorf("blob files = %d, want 1 (cross-protocol dedup)", n)
	}
}

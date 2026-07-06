package e2e

import (
	"bytes"
	"context"
	"crypto/md5"
	"errors"
	"hash/crc32"
	"io"
	"testing"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

func TestObjectRoundTrip(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	bkt := h.MustCreateBucket(ctx, "bucket1")

	content := []byte("hello, world")
	obj := bkt.Object("hello.txt")

	w := obj.NewWriter(ctx)
	w.ContentType = "text/plain"
	w.Metadata = map[string]string{"origin": "e2e"}
	if _, err := w.Write(content); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	attrs, err := obj.Attrs(ctx)
	if err != nil {
		t.Fatalf("attrs: %v", err)
	}
	if attrs.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", attrs.Size, len(content))
	}
	if attrs.ContentType != "text/plain" {
		t.Errorf("contentType = %q, want text/plain", attrs.ContentType)
	}
	if attrs.Metadata["origin"] != "e2e" {
		t.Errorf("metadata = %v, want origin=e2e", attrs.Metadata)
	}
	if attrs.Generation <= 0 {
		t.Errorf("generation = %d, want > 0", attrs.Generation)
	}
	if attrs.Metageneration != 1 {
		t.Errorf("metageneration = %d, want 1", attrs.Metageneration)
	}
	wantMD5 := md5.Sum(content)
	if !bytes.Equal(attrs.MD5, wantMD5[:]) {
		t.Errorf("md5 = %x, want %x", attrs.MD5, wantMD5)
	}
	wantCRC := crc32.Checksum(content, crc32.MakeTable(crc32.Castagnoli))
	if attrs.CRC32C != wantCRC {
		t.Errorf("crc32c = %d, want %d", attrs.CRC32C, wantCRC)
	}

	// Full read; the client verifies the crc32c from X-Goog-Hash itself.
	r, err := obj.NewReader(ctx)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("reader close: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("read %q, want %q", got, content)
	}

	// Overwrite bumps the generation.
	w = obj.NewWriter(ctx)
	if _, err := w.Write([]byte("second version")); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close v2: %v", err)
	}
	attrs2, err := obj.Attrs(ctx)
	if err != nil {
		t.Fatalf("attrs v2: %v", err)
	}
	if attrs2.Generation <= attrs.Generation {
		t.Errorf("generation did not increase: %d -> %d", attrs.Generation, attrs2.Generation)
	}

	if err := obj.Delete(ctx); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := obj.Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("attrs after delete: got %v, want ErrObjectNotExist", err)
	}
}

func TestEmptyObject(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	bkt := h.MustCreateBucket(ctx, "bucket1")

	obj := bkt.Object("empty")
	w := obj.NewWriter(ctx)
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		t.Fatalf("attrs: %v", err)
	}
	if attrs.Size != 0 {
		t.Errorf("size = %d, want 0", attrs.Size)
	}
	r, err := obj.NewReader(ctx)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil || len(got) != 0 {
		t.Errorf("read = %q, %v; want empty", got, err)
	}
}

func TestRangeRead(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	bkt := h.MustCreateBucket(ctx, "bucket1")

	content := []byte("hello, world")
	obj := bkt.Object("range.txt")
	w := obj.NewWriter(ctx)
	w.Write(content)
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	r, err := obj.NewRangeReader(ctx, 7, 5)
	if err != nil {
		t.Fatalf("range reader: %v", err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "world" {
		t.Errorf("range read = %q, want %q", got, "world")
	}

	// Suffix read: last 5 bytes.
	r2, err := obj.NewRangeReader(ctx, -5, -1)
	if err != nil {
		t.Fatalf("suffix range reader: %v", err)
	}
	defer r2.Close()
	got2, err := io.ReadAll(r2)
	if err != nil {
		t.Fatalf("suffix read: %v", err)
	}
	if string(got2) != "world" {
		t.Errorf("suffix read = %q, want %q", got2, "world")
	}
}

func TestListObjects(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	bkt := h.MustCreateBucket(ctx, "bucket1")

	for _, name := range []string{"a/1.txt", "a/2.txt", "b/3.txt", "top.txt"} {
		w := bkt.Object(name).NewWriter(ctx)
		w.Write([]byte(name))
		if err := w.Close(); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	list := func(q *storage.Query) (objects, prefixes []string) {
		t.Helper()
		it := bkt.Objects(ctx, q)
		for {
			attrs, err := it.Next()
			if err == iterator.Done {
				return
			}
			if err != nil {
				t.Fatalf("iterating: %v", err)
			}
			if attrs.Prefix != "" {
				prefixes = append(prefixes, attrs.Prefix)
			} else {
				objects = append(objects, attrs.Name)
			}
		}
	}

	objects, prefixes := list(nil)
	wantObjects := []string{"a/1.txt", "a/2.txt", "b/3.txt", "top.txt"}
	if !equalStrings(objects, wantObjects) || len(prefixes) != 0 {
		t.Errorf("flat list = %v / %v, want %v / none", objects, prefixes, wantObjects)
	}

	objects, prefixes = list(&storage.Query{Prefix: "a/"})
	if !equalStrings(objects, []string{"a/1.txt", "a/2.txt"}) {
		t.Errorf("prefix list = %v, want [a/1.txt a/2.txt]", objects)
	}

	objects, prefixes = list(&storage.Query{Delimiter: "/"})
	if !equalStrings(objects, []string{"top.txt"}) || !equalStrings(prefixes, []string{"a/", "b/"}) {
		t.Errorf("delimiter list = %v / %v, want [top.txt] / [a/ b/]", objects, prefixes)
	}
}

func TestListPagination(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	bkt := h.MustCreateBucket(ctx, "bucket1")

	var want []string
	for _, name := range []string{"p/a", "p/b", "p/c", "p/d", "p/e"} {
		w := bkt.Object(name).NewWriter(ctx)
		w.Write([]byte(name))
		if err := w.Close(); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
		want = append(want, name)
	}

	it := bkt.Objects(ctx, nil)
	pager := iterator.NewPager(it, 2, "")
	var got []string
	for {
		var page []*storage.ObjectAttrs
		nextToken, err := pager.NextPage(&page)
		if err != nil {
			t.Fatalf("paging: %v", err)
		}
		if len(page) > 2 {
			t.Errorf("page has %d items, want <= 2", len(page))
		}
		for _, attrs := range page {
			got = append(got, attrrsName(attrs))
		}
		if nextToken == "" {
			break
		}
	}
	if !equalStrings(got, want) {
		t.Errorf("paged list = %v, want %v", got, want)
	}
}

func attrrsName(attrs *storage.ObjectAttrs) string { return attrs.Name }

func TestPreconditions(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	bkt := h.MustCreateBucket(ctx, "bucket1")
	obj := bkt.Object("locked.txt")

	// First create-if-not-exists succeeds.
	w := obj.If(storage.Conditions{DoesNotExist: true}).NewWriter(ctx)
	w.Write([]byte("v1"))
	if err := w.Close(); err != nil {
		t.Fatalf("first conditional write: %v", err)
	}

	// Second one must fail with 412.
	w = obj.If(storage.Conditions{DoesNotExist: true}).NewWriter(ctx)
	w.Write([]byte("v2"))
	err := w.Close()
	if !isHTTPStatus(err, 412) {
		t.Errorf("second conditional write: got %v, want HTTP 412", err)
	}

	attrs, err := obj.Attrs(ctx)
	if err != nil {
		t.Fatalf("attrs: %v", err)
	}

	// Delete with wrong generation fails, right generation succeeds.
	err = obj.If(storage.Conditions{GenerationMatch: attrs.Generation + 1}).Delete(ctx)
	if !isHTTPStatus(err, 412) {
		t.Errorf("delete with wrong generation: got %v, want HTTP 412", err)
	}
	if err := obj.If(storage.Conditions{GenerationMatch: attrs.Generation}).Delete(ctx); err != nil {
		t.Errorf("delete with right generation: %v", err)
	}
}

// TestLargeObjectResumable exceeds the client's default 16 MiB chunk size,
// forcing the resumable upload path.
func TestLargeObjectResumable(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	bkt := h.MustCreateBucket(ctx, "bucket1")

	content := make([]byte, 20<<20) // 20 MiB
	for i := range content {
		content[i] = byte(i * 7)
	}

	obj := bkt.Object("big.bin")
	w := obj.NewWriter(ctx)
	if _, err := w.Write(content); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	attrs, err := obj.Attrs(ctx)
	if err != nil {
		t.Fatalf("attrs: %v", err)
	}
	if attrs.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", attrs.Size, len(content))
	}

	r, err := obj.NewReader(ctx)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("round-tripped content differs (len %d vs %d)", len(got), len(content))
	}
}

func TestBuckets(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()

	h.MustCreateBucket(ctx, "alpha")
	beta := h.MustCreateBucket(ctx, "beta")

	// Duplicate create conflicts.
	err := h.Client.Bucket("alpha").Create(ctx, ProjectID, nil)
	if !isHTTPStatus(err, 409) {
		t.Errorf("duplicate create: got %v, want HTTP 409", err)
	}

	attrs, err := beta.Attrs(ctx)
	if err != nil {
		t.Fatalf("bucket attrs: %v", err)
	}
	if attrs.Name != "beta" || attrs.Created.IsZero() {
		t.Errorf("bucket attrs = %+v, want name=beta and non-zero Created", attrs)
	}

	var names []string
	it := h.Client.Buckets(ctx, ProjectID)
	for {
		b, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("listing buckets: %v", err)
		}
		names = append(names, b.Name)
	}
	if !equalStrings(names, []string{"alpha", "beta"}) {
		t.Errorf("buckets = %v, want [alpha beta]", names)
	}

	// Deleting a non-empty bucket conflicts.
	w := beta.Object("x").NewWriter(ctx)
	w.Write([]byte("x"))
	if err := w.Close(); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := beta.Delete(ctx); !isHTTPStatus(err, 409) {
		t.Errorf("delete non-empty bucket: got %v, want HTTP 409", err)
	}
	if err := beta.Object("x").Delete(ctx); err != nil {
		t.Fatalf("delete object: %v", err)
	}
	if err := beta.Delete(ctx); err != nil {
		t.Errorf("delete empty bucket: %v", err)
	}
	if _, err := beta.Attrs(ctx); !errors.Is(err, storage.ErrBucketNotExist) {
		t.Errorf("attrs after delete: got %v, want ErrBucketNotExist", err)
	}
}

// TestCASDedupAndGC checks content-addressed deduplication and blob garbage
// collection by looking directly at the data directory.
func TestCASDedupAndGC(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	bkt := h.MustCreateBucket(ctx, "bucket1")

	content := []byte("identical content")
	for _, name := range []string{"copy-a", "copy-b"} {
		w := bkt.Object(name).NewWriter(ctx)
		w.Write(content)
		if err := w.Close(); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	if n := h.CountBlobFiles("bucket1"); n != 1 {
		t.Errorf("blob files after writing identical objects = %d, want 1 (dedup)", n)
	}

	w := bkt.Object("unique").NewWriter(ctx)
	w.Write([]byte("different content"))
	if err := w.Close(); err != nil {
		t.Fatalf("writing unique: %v", err)
	}
	if n := h.CountBlobFiles("bucket1"); n != 2 {
		t.Errorf("blob files after writing distinct object = %d, want 2", n)
	}

	for _, name := range []string{"copy-a", "copy-b", "unique"} {
		if err := bkt.Object(name).Delete(ctx); err != nil {
			t.Fatalf("deleting %s: %v", name, err)
		}
	}
	if n := h.CountBlobFiles("bucket1"); n != 0 {
		t.Errorf("blob files after deleting all objects = %d, want 0 (GC)", n)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func isHTTPStatus(err error, code int) bool {
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && apiErr.Code == code
}

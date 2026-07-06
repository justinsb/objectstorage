# objectstorage

A small object storage server designed to run on a NAS. It speaks (a useful
subset of) the Google Cloud Storage JSON API, so the official Google Cloud
client libraries work against it unchanged. S3 support is planned.

## Design

- **A bucket is a directory.** Easy to back up, easy to reason about.
- **Metadata in SQLite.** Each bucket has its own `data.sqlite`
  (pure-Go driver, WAL mode) holding object rows and blob refcounts.
- **Content-addressed storage.** Object content lives in
  `<bucket>/objects/<hh>/<sha256>`, deduplicated by refcount and garbage
  collected on delete. sha256, md5, and crc32c are computed once at upload.
- **One static binary, no cgo.** Cross-compiles to whatever your NAS runs.

```
<data-dir>/
  <bucket>/
    data.sqlite
    objects/ab/ab12…   # blobs, keyed by sha256
    tmp/               # in-flight uploads
```

One server process owns a data directory; run it on the NAS itself.

## Running

```sh
go build ./cmd/objectstorage
./objectstorage --data-dir /volume1/objectstorage --listen :8080
```

Point the official Go client at it:

```sh
export STORAGE_EMULATOR_HOST=nas.local:8080
```

```go
client, err := storage.NewClient(ctx) // picks up STORAGE_EMULATOR_HOST
```

## What works (V1)

- Buckets: create / get / list / delete (delete requires empty, 409 otherwise)
- Objects: upload (single-shot, multipart, and resumable — the Go client uses
  resumable for >16MiB), download with Range/suffix reads, delete, attrs
- Listing with prefix, delimiter (common prefixes), and pagination
- Generations and preconditions: `ifGenerationMatch` (incl. 0 =
  create-if-not-exists), `ifGenerationNotMatch`, metageneration variants
- Checksums: crc32c + md5 served via `X-Goog-Hash`; the official client
  verifies downloads end-to-end

Not yet: S3 protocol, auth (trusted-LAN assumption), object copy/rewrite,
metadata PATCH, versioning. See `docs/research/` for the development journal.

## Testing

End-to-end tests drive the real `cloud.google.com/go/storage` client against
an in-process server, so the whole suite is hermetic and fast:

```sh
go test ./...
```

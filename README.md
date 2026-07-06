# objectstorage

A small object storage server designed to run on a NAS. It speaks useful
subsets of both the Google Cloud Storage JSON API and the Amazon S3 API over
one shared store, so the official Google Cloud and AWS clients — and the S3
ecosystem (rclone, restic, etc.) — work against it unchanged.

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
./objectstorage --data-dir /volume1/objectstorage --listen :8080 --s3-listen :8081
```

GCS clients (port 8080):

```sh
export STORAGE_EMULATOR_HOST=nas.local:8080
```

```go
client, err := storage.NewClient(ctx) // picks up STORAGE_EMULATOR_HOST
```

S3 clients (port 8081, path-style; any credentials are accepted):

```sh
AWS_ACCESS_KEY_ID=x AWS_SECRET_ACCESS_KEY=x \
  aws --endpoint-url http://nas.local:8081 s3 ls
```

## What works

### GCS (V1)

- Buckets: create / get / list / delete (delete requires empty, 409 otherwise)
- Objects: upload (single-shot, multipart, and resumable — the Go client uses
  resumable for >16MiB), download with Range/suffix reads, delete, attrs
- Listing with prefix, delimiter (common prefixes), and pagination
- Generations and preconditions: `ifGenerationMatch` (incl. 0 =
  create-if-not-exists), `ifGenerationNotMatch`, metageneration variants
- Checksums: crc32c + md5 served via `X-Goog-Hash`; the official client
  verifies downloads end-to-end

### S3 (V2)

- Buckets: create / head / list / delete / get-location
- Objects: put (including aws-chunked streaming bodies with trailing
  checksums, as sent by modern AWS SDKs), get/head with Range and
  conditional (`If-Match`/`If-None-Match`) requests, delete (idempotent),
  batch delete, server-side copy
- Multipart uploads (initiate / upload part / complete / abort)
- ListObjects V1 and V2 with prefix, delimiter, pagination, and
  `encoding-type=url`
- ETags are the content MD5 (also for multipart objects — friendlier than
  AWS's part-hash ETags for integrity checking)

Not yet: auth of any kind (trusted-LAN assumption — do not expose to the
internet), GCS copy/rewrite and metadata PATCH, object versioning, S3
signature verification. See `docs/research/` for the development journal.

## Testing

End-to-end tests drive the real `cloud.google.com/go/storage` client against
an in-process server, so the whole suite is hermetic and fast:

```sh
go test ./...
```

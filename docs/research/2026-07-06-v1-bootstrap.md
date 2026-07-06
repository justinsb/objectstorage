# 2026-07-06: V1 bootstrap

Goal: a small object storage server for a NAS. Buckets map to directories;
each bucket has a SQLite metadata database (`<bucket>/data.sqlite`) and a
content-addressed blob store (`<bucket>/objects/<hh>/<sha256>`). Written in
Go, tested end-to-end against the official `cloud.google.com/go/storage`
client from the first commit.

## Decisions

**STORAGE_EMULATOR_HOST for the dev loop, universe domain later.** The
original idea was to test via `GOOGLE_CLOUD_UNIVERSE_DOMAIN`, but the client
then requires credentials whose universe matches, TLS, and DNS for
`storage.<domain>`. `STORAGE_EMULATOR_HOST` is the client's purpose-built
escape hatch (plain HTTP, no auth) and lets the e2e suite run hermetically on
every `go test ./...` — the server starts in-process on a random port via
`httptest`. A "sovereign universe" e2e test (self-signed TLS + fake
credentials with a custom universe) is a planned follow-up, since that is the
real deployment mode this project cares about.

**CAS with refcounts, GC inline.** Blobs are stored by sha256, sharded by the
first two hex chars. A `blobs` table carries refcounts, updated in the same
SQLite transaction as the object row; a blob file is deleted when its
refcount reaches zero. All mutations for a bucket are serialized under an
in-process `commitMu` — this makes the rename/remove of blob files and the
refcount table trivially consistent. That is fine for NAS-scale concurrency;
if it ever shows up in profiles, the lock can be narrowed (the tricky race is
a delete GC-ing a blob that a concurrent upload just deduplicated against).

**One SQLite DB per bucket, pure-Go driver.** `modernc.org/sqlite` (no cgo) so
the server cross-compiles to whatever the NAS runs. WAL mode,
busy_timeout=5s. One DB per bucket keeps lock domains independent and makes
bucket delete/backup a directory operation. Explicit assumption: exactly one
server process owns a data directory.

**Checksums computed once at upload.** GCS clients verify crc32c from the
`X-Goog-Hash` response header on full-object downloads, and S3 will need md5
for ETags, so the blob writer hashes sha256+md5+crc32c in one pass while
streaming to a temp file; rename into the CAS is the commit point.

**Generations from day one.** Per-object generation = UnixMicro (bumped if it
would not increase), metageneration starts at 1. GCS preconditions
(`ifGenerationMatch` etc.) are evaluated inside the commit transaction.
`ifGenerationMatch=0` gives create-if-not-exists (the client's
`Conditions{DoesNotExist: true}`).

**Resumable uploads are not optional.** The Go client switches to resumable
upload when content exceeds one chunk (16MiB default), so V1 implements
resumable sessions (in-memory, backed by a temp-file BlobWriter). Gotcha
discovered via e2e test: the client sends `X-GUploader-No-308: yes` and
expects "resume incomplete" as a **200 with `X-Http-Status-Code-Override:
308`**, not a real 308 (Go's http.Client treats 308 as a redirect).

**Download paths.** The client fetches media on the direct path
`/{bucket}/{object}`, not the JSON API path; we serve media on both (plus
`/download/storage/v1/...`), with `http.ServeContent` providing Range
support. Required response headers: `X-Goog-Generation` (parsed
unconditionally by the client) and `X-Goog-Hash`.

## What V1 deliberately does not do

- S3 protocol (milestone 3; the store layer is protocol-agnostic).
- Auth of any kind — trusted-LAN NAS assumption. Uniform bucket-level access
  is the world we target; no per-object ACLs ever.
- Object metadata update (PATCH), copy/rewrite/compose, object versioning
  (only the live generation is stored), notifications, lifecycle rules.
- Resumable upload session expiry (abandoned sessions leak a temp file until
  restart; sweep of `<bucket>/tmp/` at startup would fix this).
- Skipping delimiter scans server-side is implemented (common prefixes jump
  via `prefixSuccessor`), but list still scans row batches; fine at NAS scale.

## State at end of session

- `go test ./tests/e2e/` — 9/9 passing against the official GCS Go client,
  covering: round-trip with checksums/metadata, empty objects, range +
  suffix reads, list with prefix/delimiter, pagination, preconditions (412s),
  20MiB resumable upload, bucket lifecycle (409s), CAS dedup + GC.

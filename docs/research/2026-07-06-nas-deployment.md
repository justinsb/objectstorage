# 2026-07-06: First deployment to the NAS

Deployed V1 to the UGREEN NAS (`bigstore.internal`, UGOS, x86_64,
Docker 29.4.3 + Compose v5) and verified it end-to-end with the official
GCS Go client from a workstation.

## Deploy path

No registry involved: the image (~5MB, distroless static, linux/amd64) is
streamed over SSH.

```sh
docker buildx build --platform linux/amd64 -t objectstorage:v1 --load .
docker save objectstorage:v1 | ssh justinsb@bigstore.internal 'docker load'
ssh justinsb@bigstore.internal \
  'cd /volume1/docker/objectstorage && docker compose up -d'
```

Compose file lives at `/volume1/docker/objectstorage/docker-compose.yaml`
(source: `deploy/docker-compose.yaml`); data is bind-mounted at
`/volume1/docker/objectstorage/data` so it shows up in the Files app and NAS
backups.

## UGOS quirks discovered

- SSH is off by default; enabled via Control Panel. Key auth works for the
  admin user.
- The admin user is not in the `docker` group; one-time
  `sudo usermod -aG docker <user>` fixes socket access.
- **scp/SFTP is chrooted** to the share namespace, so scp'ing to an absolute
  `/volume1/...` path fails with "No such file or directory" even though the
  path exists over plain SSH. Workaround: `ssh 'cat > /path'` pipes.
- The image runs as nonroot (uid 65532), but UGOS creates bind-mount dirs
  root-owned, so the compose file overrides `user: "0:0"` for now. Chowning
  the data dir to 65532 and dropping the override would be the hardened
  version.

## Verification

From the workstation, with `STORAGE_EMULATOR_HOST=bigstore.internal:8080`,
the official client created a bucket, uploaded, and read back an object
(client-side crc32c verification passing), and the expected layout appeared
on disk: `first-bucket/data.sqlite` + `objects/0d/0d7247…` (CAS blob).

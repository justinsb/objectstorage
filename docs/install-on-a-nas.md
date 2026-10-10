# Installing on a NAS (UGREEN / UGOS)

This is the no-Docker, no-root install path: a static binary run as a
**user** systemd service. It is what is deployed today on a UGREEN NAS
running UGOS (Debian 12 base, x86_64), and it should work on any NAS that
gives you SSH, a writable home directory on the data volume, and a user
systemd instance. The earlier Docker-based deployment is described in
[`research/2026-07-06-nas-deployment.md`](research/2026-07-06-nas-deployment.md);
it stopped being an option after a NAS reset removed the Docker app, and this
path has fewer moving parts anyway.

## Prerequisites

- SSH access to the NAS with key auth (UGOS: Control Panel → Terminal → SSH).
- Go on your workstation (the version in `go.mod` or newer).
- Your home directory on the NAS should live on the data pool, not the
  system eMMC. On UGOS the personal folder is `/volume<N>/@home/<user>`, which
  `$HOME` points at; check with `df -h ~`.

You do **not** need `sudo` or Docker on the NAS.

## 1. Build a static binary on your workstation

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags='-s -w' -o /tmp/objectstorage-linux-amd64 ./cmd/objectstorage
```

Use `GOARCH=arm64` for ARM NAS models.

## 2. Copy it to the NAS

UGOS chroots scp/SFTP to the share namespace, so `scp` to an absolute path
fails even though the path exists. Pipe the file over plain SSH instead:

```sh
NAS=justinsb@192.168.2.10
ssh "$NAS" 'mkdir -p ~/objectstorage/bin ~/objectstorage/data ~/.config/systemd/user'
ssh "$NAS" 'cat > ~/objectstorage/bin/objectstorage.new' < /tmp/objectstorage-linux-amd64
ssh "$NAS" 'chmod +x ~/objectstorage/bin/objectstorage.new && mv ~/objectstorage/bin/objectstorage.new ~/objectstorage/bin/objectstorage'
```

Writing to a temporary name and renaming keeps a running server's binary
intact until the swap, which matters for upgrades (step 5).

## 3. Create the user service

```sh
ssh "$NAS" 'cat > ~/.config/systemd/user/objectstorage.service' <<'UNIT'
[Unit]
Description=objectstorage (GCS on :8080, S3 on :8081)
After=network-online.target

[Service]
ExecStart=%h/objectstorage/bin/objectstorage --data-dir=%h/objectstorage/data --listen=:8080 --s3-listen=:8081
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
UNIT
```

Then enable lingering and start it:

```sh
ssh "$NAS" 'loginctl enable-linger && systemctl --user daemon-reload && systemctl --user enable --now objectstorage'
ssh "$NAS" 'systemctl --user status objectstorage'
```

`loginctl enable-linger` is the important part. Without it, your user's
systemd instance only exists while you have a session open, so the service
would stop when you log out and would not start at boot. With it, systemd
starts your user manager at boot and brings up every enabled unit, and the
service keeps running with nobody logged in. The flag is a file in
`/var/lib/systemd/linger/`; check it is still there after a UGOS firmware
update.

Being a user service means it cannot bind ports below 1024 and its logs are
under `journalctl --user -u objectstorage`. Neither matters here.

## 4. Verify

From the NAS:

```sh
curl -s http://127.0.0.1:8081/            # S3 ListBuckets, empty <Buckets/>
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/storage/v1/b   # 200
```

From your workstation, create a bucket over S3 (any credentials are
accepted; there is no auth):

```sh
curl -X PUT http://192.168.2.10:8081/my-bucket
curl http://192.168.2.10:8081/ | grep -o '<Name>[^<]*</Name>'
```

The bucket appears on disk as `~/objectstorage/data/my-bucket/data.sqlite`
plus an `objects/` directory, and is visible in the UGOS Files app under
your personal folder, so it is included in NAS backups.

## 5. Upgrade

Rebuild (step 1), copy with the temp-name-and-rename dance (step 2), then:

```sh
ssh "$NAS" 'systemctl --user restart objectstorage'
```

## Reaching it from an IPv6-only Kubernetes cluster

If your pods have only IPv6 addresses, they cannot use the NAS's IPv4
address. The server listens on all families, so point clients at the NAS's
global IPv6 address instead. Check which ports are reachable before
concluding IPv6 is broken: UGOS firewalls SSH on IPv6 but not other ports.

A selectorless Service plus an EndpointSlice gives pods a stable name and
puts the address in one place:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: nas-objectstorage
spec:
  type: ClusterIP
  ipFamilies: [IPv6]
  ports:
    - name: s3
      port: 8081
      targetPort: 8081
---
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata:
  name: nas-objectstorage-ipv6
  labels:
    kubernetes.io/service-name: nas-objectstorage
addressType: IPv6
ports:
  - name: s3
    protocol: TCP
    port: 8081
endpoints:
  - addresses:
      - "2001:db8::1234"   # the NAS's global IPv6 address
    conditions:
      ready: true
```

Clients then use `http://nas-objectstorage.<namespace>.svc.cluster.local:8081`
with path-style addressing. When the NAS address changes, edit the
EndpointSlice and re-apply.

## Caveats

- There is no authentication. Keep the NAS on a trusted LAN and do not
  port-forward 8080/8081 from the internet.
- One process owns the data directory. Do not run a second copy against
  the same `--data-dir`.
- The server runs as your user with no sandboxing. If you later get `sudo`,
  the same unit file works as a system service in `/etc/systemd/system`
  with a `User=` line, which also removes the dependence on lingering.

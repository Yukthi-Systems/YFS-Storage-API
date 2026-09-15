# YFS Files API

A Go storage gateway that sits in front of file storage (local disk or S3/MinIO/Ceph) and handles resumable uploads, downloads, and WOPI (Office online editing) for the YFS platform.

## What it does

- Issues short-lived, encrypted session tokens (`/sessions/*`) for upload, download, and WOPI operations — backed by Redis.
- Accepts resumable uploads via the [tus](https://tus.io/) protocol and reports results back to the Rust API via a callback.
- Serves file downloads and streaming, with CORS support for browser-based clients.
- Manages file lifecycle: stat, delete, restore, purge, copy (`/files/*`), internal-only, authenticated with an API token.
- Implements the WOPI protocol so files can be opened/edited in Office Online / Collabora, etc.
- Pluggable storage backend: `local` filesystem or `s3`-compatible object storage.

## Requirements

- Go 1.25+
- Redis (session store)
- A local disk path or S3-compatible bucket for file storage

## Running locally

1. Copy the example env file and fill in real values:
   ```bash
   cp .env-example .env
   ```
2. Start Redis (if not already running).
3. Run the server:
   ```bash
   go run ./cmd/server
   ```

The server listens on `LISTEN_ADDR` (default `:8080`). See `.env-example` for the full list of configuration options (storage driver, Redis, token encryption key, Rust API callback settings, CORS, logging, etc.).

## Deployment

Deployment is automated via [.github/workflows/deploy.yaml](.github/workflows/deploy.yaml) on every push to `main` or a tag:

1. Builds a static Linux binary: `go build -o file-gateway ./cmd/server`.
2. Copies the binary to the target server over SSH/SCP.
3. Restarts the `file-gateway` systemd service on the server.

Required GitHub Actions secrets:

- `SSH_PRIVATE_KEY` — deploy key with access to the target server
- `SERVER_IP` — target server address

To deploy manually to a server running the binary as a systemd service:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o file-gateway ./cmd/server
scp file-gateway user@server:/srv/file-gateway/file-gateway.new
ssh user@server "mv /srv/file-gateway/file-gateway.new /srv/file-gateway/file-gateway && systemctl restart file-gateway"
```

Make sure the deployed environment has its own `.env` (or exported environment variables) with production values — never reuse `.env-example` values as-is.

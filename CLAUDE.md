# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A self-hosted torrent video streamer. A Go HTTP server (chi router) wraps
`anacrolix/torrent`, exposes a small REST API plus a single-page web UI, and
streams video files from a torrent to the browser while they download. The
defining feature is **space-saving storage** — it never keeps the whole file on
disk.

## Commands

```bash
go build -o phimtor2 .      # build (default CGO; prefix-cache mode only needs CGO_ENABLED=0)
go run .                    # run with defaults (listens on :8080, data in ./data)
go vet ./...                # vet
```

There are currently **no tests** in the repo.

The server serves `static/index.html` via a **cwd-relative path**
(`server.go:35`), so it must be launched from the repo root (or wherever
`static/` lives). The Docker image handles this by `WORKDIR /app` with `static/`
copied alongside the binary.

Configuration is via env vars or matching CLI flags (see `config.go`):
`PORT`, `DATA_DIR`, `READAHEAD_MB`, `STORAGE_MODE`, `PREFIX_MB`, `CACHE_MB`.

## Architecture

Flat single `main` package. The pieces that only make sense read together:

- **`TorrentManager`** (`torrent.go`) owns the `anacrolix/torrent` client and a
  map of active torrents. On every add (`AddMagnet`/`AddTorrentFile`) it spawns a
  goroutine that waits for `GotInfo()` then calls `pinPrefixPieces` to raise the
  priority of the pieces holding the first `PREFIX_MB` of each video file — so
  playback starts instantly.

- **Pluggable storage** is the core design. `newStorage` (`storage.go`) selects a
  backend by `STORAGE_MODE`:
  - `prefix-cache` (default) — a custom two-tier `storage.ClientImplCloser` in
    `storage_prefixcache.go`. One blob file per piece. **Prefix tier** (pieces
    overlapping each video's first `PREFIX_MB`) is persisted in `<DATA_DIR>/prefix`
    with a bolt completion DB and never evicted. **Cache tier** is a bounded LRU
    (`CACHE_MB`) in `<DATA_DIR>/cache`, treated as ephemeral and **wiped on
    startup**. It reports a `Capacity` to the client so evicted pieces are
    gracefully re-downloaded on later reads.
  - `capped-sqlite` — the library's built-in capped sqlite storage. **Requires
    CGO.** Selected via a build-tag pair: `storage_sqlite.go` (`//go:build cgo`)
    vs `storage_sqlite_stub.go` (`//go:build !cgo`, returns an error). A non-CGO
    build silently lacks this mode.

- **`prefixPieceIndices`** (`storage.go`) is the shared contract between the two
  worlds above: the manager uses it to set piece *priority*, and the prefix-cache
  storage uses the same function to decide which pieces route to the *persistent
  tier*. Keep these consistent — both must agree on what "the prefix" is.

- **Streaming + transcode** (`server.go` `handleStream`, `transcode.go`):
  browser-native containers (`.mp4/.webm/.ogg`) are served directly via
  `http.ServeContent` (range/seek support). Anything else is piped through an
  **`ffmpeg` subprocess** (codec copy + AAC, fragmented MP4) — so transcoding
  requires `ffmpeg` on PATH at runtime (the Docker image bundles it).

## Docker

`Dockerfile` builds a static `CGO_ENABLED=0`, amd64-only binary and runs it on a
distroless base, with a statically linked `ffmpeg` copied in (so transcoding
works while keeping the image small; `capped-sqlite` remains unavailable without
CGO).
`.github/workflows/docker.yml` builds and pushes to Docker Hub on pushes
to `main` and `v*` tags, using `DOCKERHUB_USERNAME` / `DOCKERHUB_TOKEN` secrets.
`DOCKERHUB.md` is the registry description.

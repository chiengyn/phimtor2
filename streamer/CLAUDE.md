# CLAUDE.md — streamer/

Guidance for the **torrent video streamer** service
(`module github.com/chiengyn/phimtor2/streamer`). File paths below are relative
to `streamer/`. For the repo-wide picture and the other two services, see the
root [`../CLAUDE.md`](../CLAUDE.md).

## What this is

A self-hosted torrent video streamer. A Go HTTP server (chi router) wraps
`anacrolix/torrent` and exposes a small **REST API** that streams video files
from a torrent to the browser while they download. The defining feature is
**space-saving storage** — it never keeps the whole file on disk. This service
is **backend-first**: the production watch page lives in
[`admin/`](../admin/CLAUDE.md), which calls these endpoints cross-origin (hence
the permissive CORS middleware). It does **not** touch the shared MySQL catalog.

It still ships a **minimal built-in test UI** at `GET /`
(`static/index.html`, served cwd-relative) so you can sanity-check torrent
add/list/remove/stream without the admin running. The test UI has **no
subtitle support** — that's an admin-only feature.

The API: `GET /api/torrents` (list),
`POST /api/torrents` (magnet JSON or `.torrent` multipart),
`DELETE /api/torrents/{infoHash}`, and
`GET /api/torrents/{infoHash}/files/{fileIndex}/stream`.

## Commands

```bash
go build -o phimtor2 .      # build (default CGO; prefix-cache mode only needs CGO_ENABLED=0)
go run .                    # run with defaults (listens on :8080, data in ./data)
go vet ./...                # vet
```

The test UI (`static/index.html`) is served via a **cwd-relative path**, so the
server must be launched from `streamer/` (or wherever `static/` lives) for `GET /`
to work; the API itself doesn't depend on it. It also needs `ffmpeg` on PATH for
transcoding and a writable `DATA_DIR`. The Docker image handles the cwd by
`WORKDIR /app` with `static/` copied alongside the binary.

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

**Subtitles** are *not* handled here anymore. The watch UI and the
OpenSubtitles proxy moved to [`admin/`](../admin/CLAUDE.md); the admin matches
subtitles by text query + season/episode (it has no torrent data, so no
moviehash). If you reintroduce moviehash matching it belongs here, where the
torrent reader lives.

## Docker

`Dockerfile` builds a static `CGO_ENABLED=0`, amd64-only binary and runs it on a
distroless base, with a statically linked `ffmpeg` copied in (so transcoding
works while keeping the image small; `capped-sqlite` remains unavailable without
CGO). The minimal test UI (`static/`) is copied in alongside the binary.
`.github/workflows/docker.yml` (repo root) builds and pushes all three service
images to **GHCR** (`ghcr.io/<owner>/phimtor2-{admin,viewer,streamer}`) on pushes
to `main` and `v*` tags — a matrix build, one job per service with build context
`./<service>`, authenticated with the built-in `GITHUB_TOKEN` (no extra secrets).
For deploying the stack to a host, see [`../DEPLOY.md`](../DEPLOY.md) (Kamal).

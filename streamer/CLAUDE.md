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
is **backend-first** and runs as **N interchangeable instances** behind the
[`manager/`](../manager/CLAUDE.md) control plane. It does **not** touch the
shared MySQL catalog.

**Route split (`server.go`).** Only the **data plane** is public: `GET /up`,
`GET …/{infoHash}/stats`, and `GET …/{infoHash}/files/{fileIndex}/stream`
(browsers hit the owning streamer directly for these — hence the permissive
CORS). The **control plane** (add/list/get/delete) is gated by an `internalAuth`
bearer (`STREAMER_INTERNAL_TOKEN`) and only the manager calls it. With the token
empty (single-streamer dev) the gate is a no-op and the whole API is reachable as
before.

**Self-registration (`manager_client.go`).** When `MANAGER_URL` is set, the
streamer registers with the manager on startup (advertising
`STREAMER_ADVERTISE_INTERNAL_URL` / `STREAMER_ADVERTISE_PUBLIC_URL` under
`STREAMER_INSTANCE_ID`), heartbeats every 10s, and deregisters on shutdown.
Empty `MANAGER_URL` disables this entirely (standalone mode).

It is **API-only** — there is no built-in UI (the watch page lives in the admin).

The API: `GET /api/torrents` (list), `POST /api/torrents` (magnet JSON or
`.torrent` multipart), `GET`/`DELETE /api/torrents/{infoHash}` — all **internal**
(token-gated) — plus the **public** `GET /api/torrents/{infoHash}/stats` and
`GET /api/torrents/{infoHash}/files/{fileIndex}/stream`.

## Commands

```bash
go build -o phimtor2 .      # build (default CGO; prefix-cache mode only needs CGO_ENABLED=0)
go run .                    # run with defaults (listens on :8080, data in ./data)
go vet ./...                # vet

# Concurrent-viewer load test (loadtest/). -scatter starts each viewer at a
# different offset in the same file — the realistic many-users pattern the
# multi-reader cache is built for. Reports served vs swarm download (cache
# reuse), stalls, and failures.
go run ./loadtest -magnet 'magnet:?...' -n 30 -scatter
go run ./loadtest -infohash <hex> -n 50 -bitrate 1.5 -duration 60s -scatter
```

The server needs `ffmpeg` on PATH for transcoding and a writable `DATA_DIR`.

Configuration is via env vars or matching CLI flags (see `config.go`):
`PORT`, `DATA_DIR`, `READAHEAD_MB`, `STORAGE_MODE`, `PREFIX_MB`, `CACHE_MB`,
`MAX_CONNS` (peer connections per torrent, default 200), `RETAIN_HOT`
(default off; keep every piece of a torrent that has a viewer — trades disk for
concurrent capacity), and `IDLE_TTL_MIN` (default 30; drop torrents unstreamed
for this many minutes, 0 disables). Control-plane/manager wiring (all env-only):
`STREAMER_INTERNAL_TOKEN` (gates the control routes), `MANAGER_URL` (empty
disables registration), `MANAGER_REGISTER_TOKEN`, `STREAMER_INSTANCE_ID`,
`STREAMER_ADVERTISE_INTERNAL_URL`, `STREAMER_ADVERTISE_PUBLIC_URL`.

## Architecture

Flat single `main` package. The pieces that only make sense read together:

- **`TorrentManager`** (`torrent.go`) owns the `anacrolix/torrent` client and a
  map of active torrents. On every add (`AddMagnet`/`AddTorrentFile`) it spawns a
  goroutine that waits for `GotInfo()` then calls `pinPrefixPieces` to raise the
  priority of the pieces holding the first `PREFIX_MB` of each video file — so
  playback starts instantly.

- **Idle reaper** (`reaper.go`). The manager tracks per-torrent streaming usage
  (`activity map[infoHash]*torrentActivity`: open-reader count + `lastUsed`,
  updated when a `trackedReader` opens/closes and seeded on add). A background
  goroutine drops any torrent with no open readers that has gone unstreamed for
  `IDLE_TTL_MIN`, freeing both peer connections (`t.Drop()`) and disk: the
  storage's optional `DropTorrent` (a `torrentDropper`, implemented by
  prefix-cache) removes the torrent's prefix + cache dirs, clears its bolt
  completion entries, and releases cached fds. `RemoveTorrent` (the API delete
  path) runs the same cleanup. Set `IDLE_TTL_MIN=0` to disable.

- **Pluggable storage** is the core design. `newStorage` (`storage.go`) selects a
  backend by `STORAGE_MODE`:
  - `prefix-cache` (default) — a custom two-tier `storage.ClientImplCloser` in
    `storage_prefixcache.go`. One blob file per piece. **Prefix tier** (pieces
    overlapping each video's first `PREFIX_MB`) is persisted in `<DATA_DIR>/prefix`
    with a bolt completion DB and never evicted. **Cache tier** is a bounded LRU
    (`CACHE_MB`) in `<DATA_DIR>/cache`, treated as ephemeral and **wiped on
    startup**. It reports a `Capacity` to the client so evicted pieces are
    gracefully re-downloaded on later reads.

    Eviction is **multi-reader aware**: the storage tracks *every* active
    viewer's playhead (`readers map[Hash]map[readerID]int`, fed by the
    `trackedReader` the manager wraps around each stream) and protects the
    near-ahead window of *each* reader, so many viewers watching the same file at
    different positions don't evict each other's pieces. Open blob handles are
    pooled (`storage_fdcache.go`) to cut open/close syscalls under concurrency.
    With `RETAIN_HOT` set, pieces of any torrent that still has a viewer are
    pinned (cache may exceed `CACHE_MB`), and `capFunc` grows the reported
    `Capacity` to match so the client can still request the active window.
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
  requires `ffmpeg` on PATH at runtime (the Docker image bundles it). Each stream
  reader is wrapped in a `trackedReader` (`torrent.go`) that reports its playhead
  to the storage and gets **adaptive readahead** — the per-reader readahead is
  divided down as more readers open (floor `minReadaheadBytes`) so N concurrent
  viewers don't each reserve the full `READAHEAD_MB`.

**Subtitles** are *not* handled here anymore. The watch UI and the
OpenSubtitles proxy moved to [`admin/`](../admin/CLAUDE.md); the admin matches
subtitles by text query + season/episode (it has no torrent data, so no
moviehash). If you reintroduce moviehash matching it belongs here, where the
torrent reader lives.

## Docker

`Dockerfile` builds a static `CGO_ENABLED=0`, amd64-only binary and runs it on a
distroless base, with a statically linked `ffmpeg` copied in (so transcoding
works while keeping the image small; `capped-sqlite` remains unavailable without
CGO). `.github/workflows/docker.yml` (repo root) builds and pushes all four
service images to **GHCR**
(`ghcr.io/<owner>/phimtor2-{admin,viewer,streamer,manager}`) on pushes to `main`
and `v*` tags — a matrix build, one job per service with build context
`./<service>`, authenticated with the built-in `GITHUB_TOKEN` (no extra secrets).
For deploying the stack to a host, see [`../DEPLOY.md`](../DEPLOY.md) (Kamal).

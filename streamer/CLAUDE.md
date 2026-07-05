# CLAUDE.md — streamer/

Guidance for the **torrent video streamer** service
(`module github.com/chiengyn/phimtor2/streamer`). File paths below are relative
to `streamer/`. For the repo-wide picture and the other two services, see the
root [`../CLAUDE.md`](../CLAUDE.md).

## What this is

A self-hosted torrent video streamer. A Go HTTP server (chi router) wraps
`anacrolix/torrent` and exposes a small **REST API** that streams video files
from a torrent to the browser while they download. The defining feature is
**space-saving storage** — the default `prefix-cache` mode never keeps the
whole file on disk, while `download-all` mode inverts the trade-off: it banks
the entire file up front (swarms decay — grab the data while seeders exist) and
saves space by deleting pieces viewers have already watched. This service
is **backend-first** and runs as **N interchangeable instances** behind the
[`manager/`](../manager/CLAUDE.md) control plane. It does **not** touch the
shared MySQL catalog.

**Route split (`server.go`).** Only the **data plane** is public: `GET /up`,
`GET …/{infoHash}/stats`, and `GET …/{infoHash}/files/{fileIndex}/stream`
(browsers hit the owning streamer directly for these — hence the permissive
CORS). The **control plane** (add/list/get/delete, plus `GET /api/load`) is gated by an `internalAuth`
bearer that is the streamer's **own `controlToken`** (its self-generated identity,
see `identity.go`) — only the manager holds it, having received it at registration.

**Self-registration (`manager_client.go`, `identity.go`).** The streamer **must**
register with a manager — there is no standalone mode. On first boot it
loads-or-generates a persistent identity token at `<DATA_DIR>/identity` (the
`controlToken`), then registers (advertising `STREAMER_ADVERTISE_INTERNAL_URL` /
`STREAMER_ADVERTISE_PUBLIC_URL` under `STREAMER_INSTANCE_ID`, gated by the shared
`MANAGER_REGISTER_TOKEN`, sending the `controlToken` in the body). The register
payload also self-reports the build **version** (`var version` in `main.go`,
baked via `-ldflags -X main.version=…` — the Dockerfile's `VERSION` build arg,
set by CI to the image tag + commit SHA; "dev" for local builds) and a
**settings** map (`Config.reportedSettings`: storage mode plus the knobs relevant
to it) for the admin Streamers dashboard. The manager passes settings through
opaquely, so adding a key there is all it takes to surface a new setting. An unknown
streamer is parked **pending** until an operator approves it in the admin Streamers
dashboard (the manager pins the controlToken's fingerprint on approval); until then
register returns `403` and the streamer retries. Once approved it stores the
manager-issued `sessionToken`, heartbeats every 10s, and deregisters on shutdown.
Keep `STREAMER_INSTANCE_ID` + `DATA_DIR` stable across redeploys so the pinned
approval (and identity) survive.

It is **API-only** — there is no built-in UI (the watch page lives in the admin),
and it no longer runs standalone (it must register with a manager and be approved).

The API: `GET /api/torrents` (list), `POST /api/torrents` (magnet JSON or
`.torrent` multipart), `GET`/`DELETE /api/torrents/{infoHash}`, and `GET
/api/load` (instance-wide viewer egress rate — HTTP bytes/sec served to browsers,
metered by `countingResponseWriter` in `handleStream` — plus torrent count,
polled by the manager for bandwidth-aware placement) — all **internal**
(token-gated) — plus
the **public** `GET /api/torrents/{infoHash}/stats` and
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
`PORT`, `DATA_DIR`, `READAHEAD_MB`, `STORAGE_MODE`, `PREFIX_MB`, `SUFFIX_MB`
(default 8; bytes pinned at the *end* of each video file — non-faststart MP4s
keep their moov atom there and the browser range-requests it before it can render
a frame, so pinning the tail kills a common cold-start stall), `CACHE_MB`,
`KEEP_BEHIND_MB` (default 256; download-all mode's rewind margin — watched
pieces are swept only once they fall this many MB behind every viewer's
playhead), `MAX_CONNS` (peer connections per torrent, default 200), `RETAIN_HOT`
(default off; keep every piece of a torrent that has a viewer — trades disk for
concurrent capacity), `IDLE_TTL_MIN` (default 30; drop torrents unstreamed
for this many minutes, 0 disables), `MAX_UNVERIFIED_MB` (default 0 =
unlimited; the library's global cross-torrent in-flight/unverified budget — left
off so one stalled torrent can't starve the others of piece requests, see
`NewTorrentManager` in `torrent.go`), and `STALL_TIMEOUT_SEC` (default 120, 0
disables; drop a torrent a viewer is waiting on that downloads nothing with no
connected seeders for this long — a dead swarm — so it stops pinning a peer slot
and an open reader, see `runStallChecker` in `reaper.go`). Control-plane/manager wiring (all env-only,
**all required** — no standalone mode): `MANAGER_URL`, `MANAGER_REGISTER_TOKEN`
(shared join token), `STREAMER_INSTANCE_ID`, `STREAMER_ADVERTISE_INTERNAL_URL`,
`STREAMER_ADVERTISE_PUBLIC_URL`. The control-plane credential is **not** an env
var — it is the persisted `<DATA_DIR>/identity` token.

**Patched torrent library.** `go.mod` replaces `github.com/anacrolix/torrent`
with the fork `github.com/chiengyn/torrent` (branch
`v1.61.0-reader-recursion-fix`, one commit on top of v1.61.0): upstream's
`reader.readAt` retries failed reads by unbounded recursion whenever the
storage reports a capacity (which `prefix-cache` and `download-all` both do),
so a permanently-failing read — torrent closed under a blocked stream reader
(e.g. the stall checker dropping it), cancelled context, downloads disallowed —
recurses until **stack overflow and kills the whole process** (seen in
production 2026-07-05). The patch bails out of the retry on those conditions.
Drop the replace once upstream fixes it.

## Architecture

Flat single `main` package. The pieces that only make sense read together:

- **`TorrentManager`** (`torrent.go`) owns the `anacrolix/torrent` client and a
  map of active torrents. On every add (`AddMagnet`/`AddTorrentFile`) it spawns a
  goroutine that waits for `GotInfo()` then calls `pinPrefixPieces` to raise the
  priority of the pieces holding the first `PREFIX_MB` *and* last `SUFFIX_MB` of
  each video file to `High` (the tail covers the MP4 moov atom the browser fetches
  first on a non-faststart file), and the very first piece of each video to `Now`
  (the container header) — so playback starts instantly.

- **Idle reaper** (`reaper.go`). The manager tracks per-torrent streaming usage
  (`activity map[infoHash]*torrentActivity`: open-reader count + `lastUsed`,
  updated when a `trackedReader` opens/closes and seeded on add). A background
  goroutine drops any torrent with no open readers that has gone unstreamed for
  `IDLE_TTL_MIN`, freeing both peer connections (`t.Drop()`) and disk: the
  storage's optional `DropTorrent` (a `torrentDropper`, implemented by
  prefix-cache) removes the torrent's prefix + cache dirs, clears its bolt
  completion entries, and releases cached fds. `RemoveTorrent` (the API delete
  path) runs the same cleanup. Set `IDLE_TTL_MIN=0` to disable.

- **Stall checker** (`reaper.go`, `runStallChecker`). A second background
  goroutine handles the opposite case from the reaper: a torrent someone *is*
  watching but that can't make progress. It drops any watched torrent that is
  incomplete, has downloaded no new bytes for `STALL_TIMEOUT_SEC`, **and** has no
  connected seeders (a dead/unreachable swarm) — otherwise it pins a peer slot
  and an open reader forever while the browser spins. Any byte of progress,
  completion, or a still-connected seeder (merely slow, or paused with a full
  buffer) resets its clock, so healthy playback is never dropped. Set
  `STALL_TIMEOUT_SEC=0` to disable.

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
  - `download-all` — the "bank first, sweep behind" backend
    (`storage_downloadall.go`), built because swarms decay: prefix-cache only
    fetches a window around each playhead, and on a long watch the seeders
    holding the later pieces may leave before they're ever requested. In this
    mode the manager calls **`t.DownloadAll()`** as soon as metadata arrives
    (`watchOnInfo` in `torrent.go`), racing the whole file onto disk while the
    swarm is alive — one blob per piece under `<DATA_DIR>/full` with bolt
    completion, **persistent across restarts** (nothing wiped on startup).
    Space is reclaimed from the other end by the **watched-piece sweeper**
    (`runWatchedSweeper` in `reaper.go`, 30s cadence): pieces that have fallen
    `KEEP_BEHIND_MB` behind *every* active viewer's playhead (never the pinned
    prefix/suffix) are `CancelPieces`d on the torrent, then deleted and marked
    incomplete by the storage (`SweepWatched`, via the `watchedSweeper`
    interface in `storage.go`). Torrents with no viewers are never swept —
    they're the idle reaper's job. A huge `Capacity` (1 PiB) is reported not to
    throttle but so the client treats vanished pieces as evicted and re-checks
    completion on a failed read: a viewer seeking back below the sweep cutoff
    re-downloads from the swarm on demand instead of killing the stream. Peak
    disk per torrent is the full file size; `CACHE_MB`/`RETAIN_HOT` don't apply.

- **`prefixPieceIndices`** (`storage.go`) is the shared contract between the two
  worlds above: the manager uses it to set piece *priority*, and the prefix-cache
  storage uses the same function to decide which pieces route to the *persistent
  tier*. Keep these consistent — both must agree on what "the prefix" is. It
  returns both the head (`PREFIX_MB`) and tail (`SUFFIX_MB`) pieces of each video,
  so the moov-atom tail is pinned on disk and never evicted, just like the head.

- **Streaming + transcode** (`server.go` `handleStream`, `transcode.go`):
  browser-native containers (`.mp4/.webm/.ogg`) are served directly via
  `http.ServeContent` (range/seek support). Anything else is piped through an
  **`ffmpeg` subprocess** (codec copy + AAC, fragmented MP4) — so transcoding
  requires `ffmpeg` on PATH at runtime (the Docker image bundles it). Each stream
  reader is wrapped in a `trackedReader` (`torrent.go`) that reports its playhead
  to the storage and gets **adaptive readahead** — the per-reader readahead is
  divided down as more readers open (floor `minReadaheadBytes`) so N concurrent
  viewers don't each reserve the full `READAHEAD_MB`.

  **Seeking.** Direct-play files seek natively: the browser sends a `Range`
  request at the new offset and `handleStream` calls `PrioritizeSeek` to boost the
  target pieces so the post-seek buffer fills ahead of background work (the
  dominant latency is still the swarm fetching those cold pieces). The transcode
  path is **not** seekable — it ignores `Range` and always streams sequentially
  from byte 0 (chunked, unknown-length fMP4), so a seek into a `.mkv` waits for
  the stream to reach that point. Real transcoded seeking needs HLS or a
  time-seek (`-ss`) protocol; deliberately not built yet.

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

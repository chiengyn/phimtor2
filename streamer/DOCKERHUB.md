# phimtor2 streamer

A lightweight, self-hosted torrent video **streaming API**. Add a magnet link or
`.torrent` file and stream the video straight to your browser while it
downloads — with pluggable, space-saving storage so you don't have to
keep the whole file on disk. This is a backend service (REST API only); the
companion phimtor2 admin app provides the watch UI.

## Features

- Stream torrents on the fly over HTTP with seek support (HTTP range requests)
- Add content via magnet link or `.torrent` upload
- REST API, CORS-enabled so a separate web front-end can drive it, plus a
  minimal built-in test UI at `/` for checking torrents/streaming
- Space-saving **prefix-cache** storage: pins the start of each file and keeps
  a bounded LRU cache for the rest
- On-the-fly transcoding (bundled `ffmpeg`) for non-browser-native containers
- Runs as a non-root user

## Quick start

```bash
docker run -d \
  --name phimtor2 \
  -p 8080:8080 \
  -v phimtor2-data:/data \
  <your-dockerhub-username>/phimtor2:latest
```

The API and a minimal test UI are then at http://localhost:8080. For the full
watch experience (subtitles, etc.), point the phimtor2 admin app's
`STREAMER_URL` at it.

## Configuration

Configure via environment variables (or CLI flags):

| Variable        | Default        | Description                                          |
| --------------- | -------------- | ---------------------------------------------------- |
| `PORT`          | `8080`         | HTTP server port                                     |
| `DATA_DIR`      | `/data`        | Torrent data directory (mount a volume here)         |
| `READAHEAD_MB`  | `16`           | Streaming readahead in MB                            |
| `STORAGE_MODE`  | `prefix-cache` | Storage backend (see note below)                     |
| `PREFIX_MB`     | `32`           | Bytes pinned at the start of each file, in MB        |
| `CACHE_MB`      | `2048`         | Bounded cache budget for the bulk, in MB             |

## Volumes & ports

- **`/data`** — persistent torrent cache. Mount a named volume or host path.
- **`8080`** — HTTP server (REST API + minimal test UI).

## Notes & limitations

- This image is built **without CGO**, so the `capped-sqlite` storage mode is
  not available — use the default `prefix-cache` mode.
- Browser-native formats (`.mp4`, `.webm`, `.ogg`) stream directly; other
  containers (e.g. `.mkv`, `.avi`) are transcoded on the fly via the bundled
  `ffmpeg`.
- `linux/amd64` only.

## Tags

- `latest` — most recent build of the default branch
- `<version>` (e.g. `1.2.0`, `1.2`) — released versions
- `sha-<commit>` — specific commits

## Legal

Stream only content you are legally entitled to access. You are responsible
for how you use this software.

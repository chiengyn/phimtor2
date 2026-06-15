# phimtor2

A lightweight, self-hosted torrent video streamer. Add a magnet link or
`.torrent` file and stream the video straight to your browser while it
downloads — with pluggable, space-saving storage so you don't have to
keep the whole file on disk.

## Features

- Stream torrents on the fly over HTTP with seek support (HTTP range requests)
- Add content via magnet link or `.torrent` upload
- Simple built-in web UI
- Space-saving **prefix-cache** storage: pins the start of each file and keeps
  a bounded LRU cache for the rest
- Small, static, distroless image (~22 MB), runs as a non-root user

## Quick start

```bash
docker run -d \
  --name phimtor2 \
  -p 8080:8080 \
  -v phimtor2-data:/data \
  <your-dockerhub-username>/phimtor2:latest
```

Then open http://localhost:8080.

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
- **`8080`** — HTTP server (web UI + API).

## Notes & limitations

- This image is built **without CGO**, so the `capped-sqlite` storage mode is
  not available — use the default `prefix-cache` mode.
- The image does **not** include `ffmpeg`. Browser-native formats
  (`.mp4`, `.webm`, `.ogg`) stream directly; other containers (e.g. `.mkv`,
  `.avi`) that require on-the-fly transcoding are not supported in this image.
- `linux/amd64` only.

## Tags

- `latest` — most recent build of the default branch
- `<version>` (e.g. `1.2.0`, `1.2`) — released versions
- `sha-<commit>` — specific commits

## Legal

Stream only content you are legally entitled to access. You are responsible
for how you use this software.

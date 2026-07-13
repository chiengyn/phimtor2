# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

**phimtor2** is a self-hosted movie/TV platform built from **four independent Go
services**, each its own module with its own `CLAUDE.md`. Two of them (admin,
viewer) share a single MySQL database; the streamer(s) and manager stand alone.

| Module | Purpose | Default port | Storage | Detail |
|--------|---------|--------------|---------|--------|
| **`admin/`** | TMDB importer + admin UI (writes the catalog) + torrent watch page + streamers dashboard + featured-titles curation | `8081` | MySQL (owner) | [`admin/CLAUDE.md`](admin/CLAUDE.md) |
| **`viewer/`** | Public read-only browse/discovery + watch UI | `8082` | MySQL (read-only) | [`viewer/CLAUDE.md`](viewer/CLAUDE.md) |
| **`streamer/`** | Torrent video streaming **API** (backend-only, space-saving storage); **N interchangeable instances** | `8080` | local disk / bolt / sqlite | [`streamer/CLAUDE.md`](streamer/CLAUDE.md) |
| **`manager/`** | Control plane that load-balances torrents across streamers (token-gated; public host + internal alias) | `8083` | enrollment JSON file | [`manager/CLAUDE.md`](manager/CLAUDE.md) |

The production front-end is the admin's watch page (`/watch`) and the public
viewer's watch page. **Control plane vs data plane:** the browser asks its own app
server (admin/viewer) to add/prepare a torrent; that server calls the **manager**
server-side (`MANAGER_INTERNAL_URL` + bearer token), which picks a streamer, adds
the torrent there, and returns the owning streamer's **public URL**. The browser
then hits *that* streamer directly for **stats + stream** (the only public
streamer routes; add/list/get/delete/metainfo are internal, gated by the
streamer's own self-generated identity token). When the source has a stored
`.torrent` file the app sends it on add (instead of the magnet) so the streamer
skips the slow DHT metadata fetch; magnet-only sources are backfilled by the admin
(background harvest + a manual button) from the metainfo streamers resolve once a
torrent is live. Streamers **self-enroll** with the manager: an
unknown streamer is **pending** until an operator approves it in the admin
Streamers dashboard (the manager pins its identity fingerprint), then it
heartbeats. Subtitle search (OpenSubtitles) is proxied by the admin. The streamer
is **API-only** (no UI) and **must** register with a manager — there is no
standalone mode.

When working inside a module, read **that module's `CLAUDE.md`** — file paths in
each are relative to the module directory. There are currently **no tests** in
any module.

## The shared MySQL catalog (admin ⇄ viewer)

`admin/` and `viewer/` are two ends of one database:

- **`admin/` owns the schema.** It runs the embedded migrations in
  `admin/migrations/` on startup (`admin/store.go`) and is the only writer.
- **`viewer/` only reads.** It **never migrates** and assumes the tables already
  exist (`viewer/main.go`).

So schema changes live in `admin/` (a new numbered `admin/migrations/NNNN_*.sql`),
and any new column the viewer should surface must be added to **both** modules'
`models.go`/`store.go` query layers (they are intentionally duplicated, not a
shared package). Both connect with `parseTime=true&charset=utf8mb4` so dates scan
into `time.Time` and Vietnamese text round-trips; both retry the initial ping so
they survive MySQL not being ready yet under compose.

The catalog is **Vietnamese-first with English fallback**: TMDB fields are
fetched in `vi-VN` and any empty field is backfilled from `en-US`. UI strings in
both services are Vietnamese ("Phim lẻ" = movies, "Phim bộ" = TV series).

## Repo-wide commands & layout

- All three modules: `go build .`, `go run .`, `go vet ./...` from the module dir.
  All target **Go 1.26**.
- `docker-compose.yml` (repo root) provisions the **shared MySQL 8** (utf8mb4) for
  admin/viewer. Quick start:
  ```bash
  cp admin/.env.example .env   # fill TMDB_API_KEY and ADMIN_PASSWORD
  docker compose up -d         # MySQL; admin UI at http://localhost:8081
  ```
- `data/` holds the streamer's on-disk torrent storage (gitignored).
- The **viewer** serves its UI assets via a **cwd-relative path** (`static/` +
  `templates/`), so it must be launched from its own module directory (the Docker
  image handles this with `WORKDIR /app`). The admin embeds its templates in the
  binary; the streamer and manager are API-only (no assets).

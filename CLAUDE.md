# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

**phimtor2** is a self-hosted movie/TV platform built from **three independent Go
services**, each its own module with its own `CLAUDE.md`. Two of them (admin,
viewer) share a single MySQL database; the streamer stands alone.

| Module | Purpose | Default port | Storage | Detail |
|--------|---------|--------------|---------|--------|
| **`admin/`** | TMDB importer + admin UI (writes the catalog) + torrent watch page | `8081` | MySQL (owner) | [`admin/CLAUDE.md`](admin/CLAUDE.md) |
| **`viewer/`** | Public read-only browse/discovery + watch UI | `8082` | MySQL (read-only) | [`viewer/CLAUDE.md`](viewer/CLAUDE.md) |
| **`streamer/`** | Torrent video streaming **API** (backend-only, space-saving storage) | `8080` | local disk / bolt / sqlite | [`streamer/CLAUDE.md`](streamer/CLAUDE.md) |

The production front-end is the admin's watch page (`/watch`); the browser calls
the streamer's API directly (cross-origin, so the streamer's `STREAMER_URL` must
be browser-reachable). Subtitle search (OpenSubtitles) is proxied by the admin,
not the streamer. The streamer keeps a **minimal built-in test UI** at its own
`/` (no subtitles) for sanity-checking torrents/streaming in isolation.

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
- Each service serves its UI assets via a **cwd-relative path** (`static/` or
  `templates/`), so it must be launched from its own module directory (the Docker
  images handle this with `WORKDIR /app`).

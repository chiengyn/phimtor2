# CLAUDE.md — viewer/

Guidance for the **public viewer** service
(`module github.com/chiengyn/phimtor2/viewer`). File paths below are relative to
`viewer/`. For the repo-wide picture and the other two services, see the root
[`../CLAUDE.md`](../CLAUDE.md).

## What this is

The **read side** of the shared catalog: a public, server-rendered browse /
discovery / watch UI over the movie/TV metadata that [`admin/`](../admin/CLAUDE.md)
imports. It renders Go `html/template` pages, enhanced with htmx (live filtering)
and Alpine.js, in Vietnamese.

It is **strictly read-only**: it never writes and **never runs migrations** —
[`admin/`](../admin/CLAUDE.md) is the sole owner of the schema. The viewer assumes
the tables already exist.

## Commands

```bash
go build -o viewer .   # build (static CGO_ENABLED=0 binary)
go run .               # run (listens on :8082, needs the shared MySQL)
go vet ./...
```

Templates (`templates/`) and `static/` are loaded via **cwd-relative paths**
(`server.go`), so run from `viewer/`. Targets Go 1.26. There is no Dockerfile yet.

## Configuration (`config.go`)

Env vars with matching CLI flags. No secrets — there is no auth (it's a public
service).

- HTTP: `VIEWER_PORT` (8082).
- MySQL: `MYSQL_DSN` (overrides the rest) or `DB_HOST`/`DB_PORT`/`DB_USER`/
  `DB_PASSWORD`/`DB_NAME`. Same `parseTime=true&charset=utf8mb4` DSN as admin.

## Architecture

Flat single `main` package.

- **Templates** (`server.go` `parseTemplates`, `templates/`): parsed once at
  startup into named sets, each layered on `layout.html`. `home.html` composes
  `rows.html` (browse) and `grid.html` (filtered results); `grid.html` also
  defines the shared `card` partial. A small `funcMap` provides `img` (TMDB image
  URLs — empty path ⇒ `""` so templates fall back to a placeholder), `year`
  (4-digit year from a `YYYY-MM-DD` string), and `rating` (one decimal).
  `tmdbImageBase` builds poster/backdrop URLs client-unaware of TMDB.

- **Routes** (`server.go` `setupRouter`):
  - `GET /` — home. With an active filter (`q`/`genre`/`type`) it renders a flat
    **grid**; otherwise Netflix-style **rows**.
  - `GET /titles` — the grid **fragment only**, for htmx swaps as filters change.
  - `GET /titles/{id}` — full detail page (genres, and for TV its seasons/episodes).
  - `GET /watch/movie/{id}` and `GET /watch/episode/{id}` — the watch page.
  - `/static/*` — static assets (`style.css`).
  Unknown / bad ids render the `404.html` page (not a bare error).

- **Watch page is mocked.** `handleWatchMovie`/`handleWatchEpisode` resolve the
  title/episode for headings but play a fixed `mockVideoURL` (Big Buck Bunny).
  Real playback (wiring to [`streamer/`](../streamer/CLAUDE.md)) is not yet built —
  this is the obvious place that will change.

- **Domain types** (`models.go`): `Title` → `Genre`/`Season` → `Episode`, mirroring
  the admin catalog. Dates are `"YYYY-MM-DD"` strings. These are deliberately a
  separate copy from admin's (no shared package).

- **Store** (`store.go`): read-only `database/sql` queries.
  - `ListTitles(filter)` — discovery list with optional free-text title `LIKE`,
    genre, and type constraints, newest first.
  - `ListRows` — the browse home: loads every title once, then buckets into rows
    (movies, then TV, then one row per genre), keeping each row newest-first with a
    single pass over the title order. Empty rows are omitted.
  - `ListGenres` — only genres attached to at least one title (filter dropdown).
  - `GetTitle` — full title with genres and (TV) seasons+episodes; `(nil, nil)` on
    miss.
  - `GetEpisodeContext` — resolves an episode id to its parent title and
    season/episode numbers for the watch heading.

When `admin/` adds a column you want to surface, add it here too — the query layers
are intentionally duplicated, not shared.

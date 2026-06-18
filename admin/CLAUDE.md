# CLAUDE.md — admin/

Guidance for the **TMDB metadata admin** service
(`module github.com/chiengyn/phimtor2/admin`). File paths below are relative to
`admin/`. For the repo-wide picture and the other two services, see the root
[`../CLAUDE.md`](../CLAUDE.md).

## What this is

The **write side** of the shared catalog. An admin pastes a themoviedb.org id or
link; the service fetches movie/TV metadata from TMDB (Vietnamese, English
fallback) and upserts it into MySQL. It serves a tiny single-page admin UI
(`static/index.html`) behind HTTP Basic auth. The public, read-only
[`viewer/`](../viewer/CLAUDE.md) renders what this service writes.

This module **owns the MySQL schema** and is the only writer. It runs the
embedded migrations on startup; the viewer never migrates.

## Commands

```bash
go build -o phimtor2-admin .   # build (static CGO_ENABLED=0 binary, see Dockerfile)
go run .                       # run (listens on :8081, needs MySQL + env)
go vet ./...
```

`static/index.html` is served via a **cwd-relative path** (`server.go`), so run
from `admin/`. Targets Go 1.26.

## Configuration (`config.go`)

Env vars with matching CLI flags, except the two **secrets which are env-only**:
`TMDB_API_KEY` and `ADMIN_PASSWORD` — both **required** (`main.go` fatals if
unset).

- HTTP: `ADMIN_PORT` (8081), `ADMIN_USER` (admin), `ADMIN_PASSWORD`.
- TMDB: `TMDB_API_KEY`, `TMDB_LANGUAGE` (`vi-VN`), `TMDB_FALLBACK_LANGUAGE` (`en-US`).
- MySQL: `MYSQL_DSN` (overrides the rest when set) or `DB_HOST`/`DB_PORT`/
  `DB_USER`/`DB_PASSWORD`/`DB_NAME`. The DSN is built with
  `parseTime=true&charset=utf8mb4` so dates scan into `time.Time` and Vietnamese
  text round-trips.

## Architecture

Flat single `main` package. Layers, in request order:

- **HTTP** (`server.go`): chi router; `s.basicAuth` (constant-time compare) gates
  **every** route — both the UI and the API. Endpoints: `POST /api/import`,
  `GET /api/titles`, `GET /api/titles/{id}`, `DELETE /api/titles/{id}`.

- **Ref parsing** (`ref.go`): `parseRef` turns the admin's input into a
  `(mediaType, id)` pair. A themoviedb.org link carries its own type
  (`/movie/27205`, `/tv/1399`); a bare numeric id requires the UI's type dropdown.

- **TMDB client** (`tmdb.go`): fetches in the primary language and backfills any
  empty field (title, overview, genres; per-season and per-episode names/overviews
  for TV) from **one extra request** in the fallback language. For TV it walks
  every season to pull episodes. Only the fields actually stored are unmarshalled.

- **Domain types** (`models.go`): `Title` → `Genre`/`Season` → `Episode`. Shared
  by the TMDB client (builds them), the store (persists/loads), and HTTP
  (serializes). **Dates are `"YYYY-MM-DD"` strings** (empty = unknown) to map
  cleanly onto both the MySQL `DATE` columns and JSON.

- **Store** (`store.go`): `database/sql` over `go-sql-driver/mysql`.
  - `UpsertTitle` is one transaction: upserts the title (`ON DUPLICATE KEY UPDATE`
    keyed on `uniq_tmdb (tmdb_id, type)`, using `id = LAST_INSERT_ID(id)` to get
    the existing id back), replaces its genre links wholesale, then upserts
    seasons and re-inserts episodes. Deletes cascade via FKs.
  - Empty strings/dates are stored as SQL `NULL` (`nullStr`/`dateArg`).

## Migrations (`store.go` + `migrations/`)

Migrations are **embedded** (`//go:embed migrations/*.sql`) and applied on startup
in filename order, each recorded in `schema_migrations` so it runs once. Rules:

- Add changes as a **new numbered file** (`migrations/0002_add_foo.sql`). **Never
  edit an already-applied file.**
- The migrator splits each file on `;` and runs statements individually (after
  stripping `--` line comments), so **never put a semicolon anywhere but a
  statement terminator** — not even inside a comment.
- The schema (`0001_init.sql`) is `utf8mb4`/`utf8mb4_unicode_ci` throughout:
  `titles` (+`uniq_tmdb`), `genres`, `title_genres`, `seasons`, `episodes`, with
  cascading FKs.

A new column the viewer should display must also be added to **`viewer/`'s** own
`models.go`/`store.go` — the two services duplicate their query layers rather than
share a package.

## Docker

`Dockerfile` builds a static `CGO_ENABLED=0` amd64 binary on a distroless base,
copying `static/` next to it under `WORKDIR /app`. Brought up together with MySQL
via the repo-root `docker-compose.yml`.

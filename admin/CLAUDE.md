# CLAUDE.md — admin/

Guidance for the **TMDB metadata admin** service
(`module github.com/chiengyn/phimtor2/admin`). File paths below are relative to
`admin/`. For the repo-wide picture and the other two services, see the root
[`../CLAUDE.md`](../CLAUDE.md).

## What this is

The **write side** of the shared catalog. An admin pastes a themoviedb.org id or
link; the service fetches movie/TV metadata from TMDB (Vietnamese, English
fallback) and upserts it into MySQL. It serves a tiny **htmx + Alpine** admin UI
(server-rendered `html/template`, embedded in the binary) behind HTTP Basic auth.
The public, read-only [`viewer/`](../viewer/CLAUDE.md) renders what this service
writes.

This module also hosts the **torrent watch page** (`GET /watch`,
`templates/watch.html`). The page itself is a plain JS SPA that talks to the
[`streamer/`](../streamer/CLAUDE.md) service **directly from the browser**
(cross-origin) for torrent add/list/remove/stream — its base URL is injected via
`STREAMER_URL`. Subtitles are served by **this** service: `opensubtitles.go` is a
server-side proxy (so the API key never reaches the browser) that searches by
text query + season/episode parsed from the file name. There is no moviehash
matching here — the admin holds no torrent data; that lives in the streamer.

This module **owns the MySQL schema** and is the only writer. It runs the
embedded migrations on startup; the viewer never migrates.

## Commands

```bash
go build -o phimtor2-admin .   # build (static CGO_ENABLED=0 binary, see Dockerfile)
go run .                       # run (listens on :8081, needs MySQL + env)
go vet ./...
```

The UI templates (`templates/*.html`) are **embedded** in the binary
(`//go:embed` in `server.go`), so the binary is self-contained and not
cwd-dependent. Targets Go 1.26.

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
- Watch page: `STREAMER_URL` (`http://localhost:8080`) — must be reachable from
  the **browser**, not just the admin server, since the page calls it directly.
- OpenSubtitles (env-only, like the other secrets; no flags): `OPENSUBTITLES_API_KEY`
  (required to enable subtitle search/download), `OPENSUBTITLES_USER_AGENT`, and
  optional `OPENSUBTITLES_USERNAME` / `OPENSUBTITLES_PASSWORD` (a login token
  raises the per-day download quota).
- Subtitle storage (`blobstore.go`): saved subtitle files are written to a
  `BlobStore`. `SUBTITLE_STORAGE_BACKEND` selects `local` (default) or `s3`.
  Local uses `SUBTITLE_STORAGE_DIR` (default `./data/subtitles`, gitignored). S3
  uses `S3_ENDPOINT`/`S3_REGION`/`S3_BUCKET`/`S3_ACCESS_KEY`/`S3_SECRET_KEY`/
  `S3_USE_SSL` (the s3 store is only built when `S3_BUCKET` is set; secrets are
  env-only). The local store is always available, so reads/deletes route by each
  row's recorded `storage_backend` even if the default later changes. A MinIO
  service for testing is in the root compose under the `s3` profile.

## Architecture

Flat single `main` package. Layers, in request order:

- **HTTP** (`server.go`): chi router; `s.basicAuth` (constant-time compare) gates
  **every** route. The UI is **htmx-driven**, so most endpoints return HTML
  fragments rendered from the embedded `templates/*.html` (`render`/`renderMsg`),
  not JSON:
  - `GET /` — full page (`index.html`), list pre-rendered for first paint.
  - `POST /api/import` — form-encoded (`ref`, `type`); returns the `#msg`
    fragment and, on success, sets `HX-Trigger: titlesChanged` so the list
    re-fetches itself. Always 200 (htmx skips swaps on error codes; the `err`
    CSS class signals failure).
  - `GET /api/titles` — the titles list fragment (swapped into `#list`).
  - `DELETE /api/titles/{id}` — empty **200** (not 204, which htmx ignores) so
    the card's `outerHTML` swap removes the row.
  - `GET /api/titles/{id}` — the one remaining **JSON** endpoint (full title;
    currently unused by the UI).
  - `GET /watch` — the torrent watch page (`watch.html`), with `STREAMER_URL`
    and a `SubtitlesEnabled` flag injected via `<body data-*>` (the page is plain
    JS, not htmx).
  - `GET /api/subtitles/search` (`?file=&query=&languages=&season=&episode=`) and
    `GET /api/subtitles/download` (`?file_id=`) — the live subtitle-provider proxy
    (`opensubtitles.go`, behind the `SubtitleProvider` interface so more providers
    can be registered); search returns JSON, download returns WebVTT.
  - `POST /api/subtitles` (JSON) — download a search hit from its provider, store
    the file in the primary `BlobStore`, and record a `subtitles` row attached to
    a movie (`title_id`) or episode (`episode_id`); fires `HX-Trigger:
    subtitlesChanged`. `GET /api/subtitles/{id}/file` serves the stored file;
    `DELETE /api/subtitles/{id}` removes row + blob. `GET
    /api/titles/{id}/subtitles` returns the `subtitle-region` fragment.
  - `POST /api/subtitles/upload` (multipart `file`, `language`, `name`,
    `title_id`/`episode_id`) — the **manual upload** path: the admin uploads a
    `.srt`/`.vtt` from their computer. Unlike the search/download path it needs no
    `SubtitleProvider` (only a `BlobStore`), so it works even when OpenSubtitles
    is unconfigured. The **original bytes are stored verbatim** (SubRip is kept
    as-is, not converted) with the detected `format` (`srt`/`vtt`) on the row and
    `provider = "manual"`; the play page converts SRT to WebVTT client-side on
    load (the saved-subtitle summary now carries `format`).
  - The subtitle search/select/save UI lives on three surfaces: the add-torrent
    page, the title detail page (a modal, with saved subtitles listed per
    movie/episode), and the play page (which also auto-loads saved subtitles). The
    OpenSubtitles **search** section on the first two is gated by the
    `SubtitlesEnabled` flag; the **manual upload** section is always shown.

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
    seasons and episodes in place (keyed on `uniq_episode (season_id,
    episode_number)`, preserving episode ids so a metadata refresh keeps the
    videos/subtitles attached to them). Deletes cascade via FKs.
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
  cascading FKs. Later migrations add `torrent_sources`/`videos` (`0003`) and
  `subtitles` (`0004`). Both `videos` and `subtitles` use the same owner pattern:
  exactly one of `title_id`/`episode_id` is set (a `CHECK` enforces it), FKs
  cascade. `subtitles` keeps only the file's locator (`storage_backend` +
  `storage_key`) — the bytes live in a `BlobStore`, not MySQL — plus the provider
  metadata (`provider`, `provider_file_id`, `language`, `download_count`, and a
  JSON `metadata` for extras).

A new column the viewer should display must also be added to **`viewer/`'s** own
`models.go`/`store.go` — the two services duplicate their query layers rather than
share a package.

## Docker

`Dockerfile` builds a static `CGO_ENABLED=0` amd64 binary on a distroless base,
copying `static/` next to it under `WORKDIR /app`. Brought up together with MySQL
via the repo-root `docker-compose.yml`.

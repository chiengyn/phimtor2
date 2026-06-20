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
- Streamer (watch page): `STREAMER_PUBLIC_URL` (8080) — **browser-reachable**,
  injected into the watch page for the streamer's stats + stream endpoints; and
  `STREAMER_INTERNAL_URL` (8080) — server-to-server, used by the viewer to **add**
  torrents (e.g. `http://streamer:8080` under compose). In local dev they're
  usually identical.
- Subtitle storage (`blobstore.go`, **read-only**): the viewer reads the *same*
  storage the admin writes to. `SUBTITLE_STORAGE_BACKEND` (`local`|`s3`),
  `SUBTITLE_STORAGE_DIR` (`./data/subtitles` — for `local` this **must** be the
  same directory admin writes to, a shared volume under compose), and the same
  `S3_*` vars as admin (used with the same bucket). Only `Get` is implemented;
  the viewer never writes or deletes subtitle files.

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
  - `POST /api/sources/{videoID}/prepare` — viewer-mediated playback (see below).
  - `GET /api/subtitles/{id}/file` — serves a saved subtitle file read-only from
    the shared blob store, by the row's `storage_backend` + `storage_key`.
  - `/static/*` — static assets (`style.css`).
  Unknown / bad ids render the `404.html` page (not a bare error).

- **Watch page plays real torrents, viewer-mediated.** `handleWatchMovie`/
  `handleWatchEpisode` resolve the videos (`VideosForTitle`/`VideosForEpisode`,
  newest first — first entry is the default) and saved subtitles, and inject them
  (plus `STREAMER_PUBLIC_URL`) into `watch.html` as JSON in `data-*` attributes.
  The page (`templates/watch.html`, a Plyr-based plain-JS player) **never adds
  torrents directly**: it `POST`s to the same-origin `/api/sources/{id}/prepare`,
  which adds the magnet to the streamer **server-to-server** (`streamer.go`) and
  returns `{infoHash, fileIndex}`. The browser then streams from, and polls
  `…/stats` on, the streamer's **public** endpoints only. The stats poll feeds a
  user-facing **progress bar** plus a collapsed **debug panel** (speeds/peers —
  not meant for end users). A **source selector** appears when more than one
  video exists. Saved subtitles are listed as chips (first auto-loaded); the user
  can also load a local `.srt`/`.vtt`. There is no OpenSubtitles search here (the
  viewer is read-only and holds no provider key).

- **Streamer client** (`streamer.go`): a tiny server-side HTTP client whose only
  job is `addTorrent(magnet) → infoHash`. Keeping the add on the server means only
  the streamer's stats + stream endpoints need to be browser-reachable (the rest
  can be firewalled internal in deployment; the streamer itself is unchanged).

- **Subtitle blob store** (`blobstore.go`): a **read-only** port of admin's store
  (`Get` only, `local` + `s3`); `handleSubtitleFile` routes a subtitle row to
  `s.blobs[storage_backend]` and serves the bytes (`errBlobNotFound` → 404).

- **Domain types** (`models.go`): `Title` → `Genre`/`Season` → `Episode`, plus
  `Video` and `Subtitle` (mirroring admin; `Video.Magnet` is `json:"-"` so it is
  never serialized to the browser). Dates are `"YYYY-MM-DD"` strings. Deliberately
  a separate copy from admin's (no shared package).

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
  - `VideosForTitle`/`VideosForEpisode` — playable videos for the owner (join
    `torrent_sources` for `info_hash`/`magnet`), newest first; `GetVideo` — one
    video for the prepare endpoint.
  - `SubtitlesForTitle`/`SubtitlesForEpisode` + `GetSubtitle` — saved subtitle
    rows for the watch page and file endpoint.

When `admin/` adds a column you want to surface, add it here too — the query layers
are intentionally duplicated, not shared.

# CLAUDE.md — viewer/

Guidance for the **public viewer** service
(`module github.com/chiengyn/phimtor2/viewer`). File paths below are relative to
`viewer/`. For the repo-wide picture and the other two services, see the root
[`../CLAUDE.md`](../CLAUDE.md).

## What this is

The **read side** of the shared catalog: a public, server-rendered browse /
discovery / watch UI over the movie/TV metadata that [`admin/`](../admin/CLAUDE.md)
imports. It renders Go `html/template` pages in Vietnamese. The browse/discovery
flow is **fully server-rendered with no JS framework**: filtering is a GET
`<form>` and pagination is plain `<a>` links, so every state has a real,
shareable URL (search/genre/type/page all live in the query string). Only the
Plyr-based watch page carries page-specific JS.

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
- Streamer manager (watch page): `MANAGER_INTERNAL_URL` (`http://localhost:8083`)
  + `MANAGER_INTERNAL_TOKEN` (env-only bearer) — server-to-server. The viewer adds
  torrents via the manager (`manager.go`); the manager returns the owning
  streamer's **public URL**, which the prepare response hands to the browser for
  stats + stream directly. There is no static public streamer URL anymore.
- Watch-session reaping: `WATCH_HEARTBEAT_TTL` (30s) — how long a watch session
  may go silent before the viewer drops its torrent (via the manager) to free
  streamer resources. Keep it well above the watch page's 10s heartbeat interval.
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
  `rows.html` (browse) and `grid.html` (filtered results + the `pager`);
  `grid.html` also defines the shared `card` partial. A small `funcMap` provides
  `img` (TMDB image URLs — empty path ⇒ `""` so templates fall back to a
  placeholder), `year` (4-digit year from a `YYYY-MM-DD` string), and `rating`
  (one decimal). `tmdbImageBase` builds poster/backdrop URLs client-unaware of
  TMDB. `detail.html` uses native `<details>` for season collapse (no JS).

- **Routes** (`server.go` `setupRouter`):
  - `GET /` — home, fully server-rendered. With an active filter
    (`q`/`genre`/`type`) it renders a paginated **grid** (`?page=N`, 1-based,
    clamped server-side); otherwise Netflix-style **rows**. `handleHome` first
    **redirects to the canonical URL** (`homeURL`): it drops empty/odd query
    params the GET filter form submits, forces page 1 in browse mode, and snaps
    an out-of-range `page` back to the last page — so the address bar always
    shows the clean, shareable URL for the current state.
  - `GET /titles/{id}` — full detail page (genres, and for TV its seasons/episodes).
  - `GET /watch/movie/{id}` and `GET /watch/episode/{id}` — the watch page.
  - `POST /api/sources/{videoID}/prepare` — viewer-mediated playback (see below).
  - `GET /api/subtitles/{id}/file` — serves a saved subtitle file read-only from
    the shared blob store, by the row's `storage_backend` + `storage_key`.
  - `POST /api/watch/heartbeat` and `POST /api/watch/leave` — watch-session
    liveness (see *Watch-session reaping* below); drop a torrent once its last
    viewer goes away.
  - `/static/*` — static assets (`style.css`).
  Unknown / bad ids render the `404.html` page (not a bare error).

- **Watch page plays real torrents, viewer-mediated.** `handleWatchMovie`/
  `handleWatchEpisode` resolve the videos (`VideosForTitle`/`VideosForEpisode`,
  newest first — first entry is the default) and saved subtitles, and inject them
  into `watch.html` as JSON in `data-*` attributes. The page
  (`templates/watch.html`, a Plyr-based plain-JS player) **never adds torrents
  directly**: it `POST`s to the same-origin `/api/sources/{id}/prepare`, which adds
  the magnet via the manager **server-to-server** (`manager.go`) and returns
  `{infoHash, fileIndex, streamerPublicURL}`. The browser then streams from, and
  polls `…/stats` on, **that streamer's** public endpoints directly. The stats poll feeds a
  user-facing **progress bar** plus a collapsed **debug panel** (speeds/peers —
  not meant for end users). A **source selector** appears when more than one
  video exists. Saved subtitles are listed as chips (first auto-loaded); the user
  can also load a local `.srt`/`.vtt`. There is no OpenSubtitles search here (the
  viewer is read-only and holds no provider key).

- **Watch-session reaping** (`watchtracker.go`, `manager.go`
  `deleteTorrent`). So a torrent doesn't linger after the user leaves (wasting the
  streamer's peers/cache/disk until its ~30-min idle reaper), the watch page
  heartbeats `POST /api/watch/heartbeat` every 10s with a per-tab `sessionID` +
  the playing `infoHash`, and beacons `POST /api/watch/leave` on `pagehide` (tab
  close, navigating to another title, mobile bfcache). The server's `watchTracker`
  **reference-counts** sessions per infohash: when the last viewer leaves (beacon)
  or goes silent past `WATCH_HEARTBEAT_TTL` (a background sweep), it drops the
  torrent via the manager (`DELETE /api/torrents/{hash}`, idempotent → routed to
  the owning streamer). Reference-counting is what makes one user leaving safe
  while others keep watching the same torrent; a source switch re-points the
  session's heartbeat, dropping the previously-watched torrent. The sweep loop is
  started from `main.go` (`server.watcher.run`) and stops on shutdown.

- **Manager client** (`manager.go`): a tiny server-side HTTP client against the
  manager (with the internal bearer token): `addTorrent(magnet) → (infoHash,
  streamerPublicURL)` and `deleteTorrent(infoHash)` (used by the watch-session
  reaper above). The manager picks a streamer on add; the returned public URL
  flows through the prepare response so the browser streams from the right
  instance. Keeping these on the server means only the streamers' stats + stream
  endpoints are browser-reachable; everything else is internal.

- **Subtitle blob store** (`blobstore.go`): a **read-only** port of admin's store
  (`Get` only, `local` + `s3`); `handleSubtitleFile` routes a subtitle row to
  `s.blobs[storage_backend]` and serves the bytes (`errBlobNotFound` → 404).

- **Domain types** (`models.go`): `Title` → `Genre`/`Season` → `Episode`, plus
  `Video` and `Subtitle` (mirroring admin; `Video.Magnet` is `json:"-"` so it is
  never serialized to the browser). Dates are `"YYYY-MM-DD"` strings. Deliberately
  a separate copy from admin's (no shared package).

- **Store** (`store.go`): read-only `database/sql` queries.
  - `ListTitles(filter, limit, offset)` — one **page** of the discovery list
    (optional free-text title `LIKE`, genre, and type constraints), newest first.
    `CountTitles(filter)` returns the total for the same filter so the grid can
    render a numbered pager; both share `titleFilterClause` so the page and the
    count always agree.
  - `ListRows` — the browse home: loads every title once, then buckets into rows
    (movies, then TV, then one row per genre), keeping each row newest-first with a
    single pass over the title order. Empty rows are omitted; each row is capped at
    `rowLimit` (the carousel's heading links to the paginated grid for the rest).
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

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
shareable URL (search/genre/type/page all live in the query string). The only
JS is plain vanilla, no build step: the Plyr-based watch page's inline script,
small inline carousel helpers, `static/bookmarks.js` (loaded site-wide from
`layout.html`, see *Bookmarks* below), and `static/mkvplayer.js` (an ES module the
watch page `import()`s only when it meets a container the browser cannot demux —
see *Playing `.mkv`* below).

It **owns no schema and never runs migrations** —
[`admin/`](../admin/CLAUDE.md) is the sole owner, and the viewer assumes the
tables already exist. It is **read-only for the whole catalog** (`titles`,
`videos`, `subtitles`, `featured_titles`, …).

The one exception, added with accounts: the viewer is the **only writer of two
tables it does not own** — `users` (created/refreshed on login) and
`user_bookmarks` (the saved list). Both are created by
`admin/migrations/0007_users.sql`. Nothing else here writes, ever.

> Deploy ordering matters because of this: the admin must apply `0007` **before**
> a viewer that writes those tables boots. The failure is contained by design —
> `currentUser` and the auth handlers treat a store error as "anonymous" and log
> it, so an un-migrated database breaks sign-in only, not the site.

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
- Google sign-in (`googleauth.go`): `GOOGLE_CLIENT_ID` + `GOOGLE_CLIENT_SECRET`
  (env-only secrets). **Both empty ⇒ accounts are off**: the `/auth/google/*`
  routes are not registered and the header shows no login button, so the site is
  exactly the anonymous-only one it was before accounts existed. That is also the
  safe rollback. The OAuth `redirect_uri` is *derived* from `VIEWER_PUBLIC_URL`
  (`<origin>/auth/google/callback`, falling back to `http://localhost:<port>/…`),
  so there is no separate env var to keep in sync — but it must match an
  "Authorized redirect URI" in the Google Cloud Console **byte for byte**.
- Session cookie (`session.go`): `SESSION_SECRET` (env-only secret,
  `openssl rand -hex 32`, ≥32 chars). **Required once `VIEWER_PUBLIC_URL` is
  set** — `NewServer` errors out rather than let production run on a key that
  changes every boot. Unset locally it generates an ephemeral key and logs a
  warning, so sign-in works with zero setup. Rotating it **logs every user out**,
  and since there is no sessions table that is the *only* revocation lever — not
  a routine credential rotation.
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
    shows the clean, shareable URL for the current state. In browse mode it also
    builds the **hero billboard carousel** from the admin's manually-curated
    `featured_titles` (`FeaturedTitleIDs`, in curated order), each reloaded in full
    via `GetTitle` for its backdrop/overview; when nothing is featured it **falls
    back** to the first row's top (score-ranked) titles so the hero is never empty.
    The curation itself lives in the admin (`GET /featured`), which owns the table.
  - `GET /titles/{id}` — full detail page (genres, and for TV its seasons/episodes).
  - `GET /bookmarks` — the "xem sau" list, in **two modes** (see *Bookmarks*
    below). Anonymous: a static shell filled client-side from localStorage.
    Signed in: server-rendered from `user_bookmarks` with the same `card`
    partial the grid uses. `noindex` in both, and deliberately absent from
    `sitemap.xml`.
  - `GET /watch/movie/{id}` and `GET /watch/episode/{id}` — the watch page.
  - `POST /api/sources/{videoID}/prepare` — viewer-mediated playback (see below).
  - `GET /api/subtitles/{id}/file` — serves a saved subtitle file read-only from
    the shared blob store, by the row's `storage_backend` + `storage_key`.
  - `GET /auth/google/start` and `GET /auth/google/callback` — sign-in
    (registered only when accounts are configured); `POST /auth/logout` —
    always registered, and POST-only so no prefetch can sign anyone out.
  - `/api/bookmarks/*` — the saved-titles API, behind `requireUser` (401 JSON
    for anonymous callers): `GET /` (ids), `DELETE /` (clear all),
    `POST|DELETE /{titleID}`, and `POST /merge` (fold a pre-login localStorage
    list into the account).
  - `POST /api/watch/heartbeat` and `POST /api/watch/leave` — watch-session
    liveness (see *Watch-session reaping* below); drop a torrent once its last
    viewer goes away.
  - `/static/*` — static assets (`style.css`, `bookmarks.js`).
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

- **Quality tiers** (`server.go`, `resolutionLock`). Sources are always *listed*
  — seeing that a better quality exists is the whole point — but not always
  playable. Three tiers, from one predicate:

  | | anonymous | signed in |
  |---|---|---|
  | 720p | plays | plays |
  | 1080p (`memberResolutions`) | 🔒 sign-in chip | plays |
  | 2160p (`lockedResolutions`) | 🔒 "sắp ra mắt" | 🔒 "sắp ra mắt" |

  1080p is gated to push registration; 4K is held back for the future paid tier
  (`User.Plan` is the seam for that, and is still read nowhere). `resolutionLock`
  returns `lockNone`/`lockMember`/`lockPaid` and is the **single source of
  truth** for the chip copy, the default-source pick, and the enforcement.
  - **The chips are client-rendered and bypassable — `handlePrepareSource` is
    the only thing that actually enforces this.** It answers `401` for a member
    lock and `403` for a paid one; the page turns that `401` into the sign-in
    gate, which is also how a session expiring mid-watch recovers.
  - `resolutionLock` is a **method on `*Server`** because the member tier must be
    inert when accounts are disabled (`s.google.enabled()`). Without that clause
    a `GOOGLE_CLIENT_ID=""` deploy — the documented rollback — would strand every
    visitor at 720p with no login button to escape it. **Test that config
    whenever you touch this.**
  - `watchData` carries `LoginURL` because `layout.html` invokes the page body as
    `{{block "content" .Data}}`: inside `watch.html` neither `.User` nor
    `$.LoginURL` is in scope. `s.loginURL(r)` builds it for both the envelope and
    the watch handlers.

- **Playing `.mkv`** (`static/mkvplayer.js`, `templates/watch.html`). Browsers do
  not demux Matroska, and the streamer's ffmpeg fallback produces a chunked
  fMP4 that ignores `Range` — so seeking a `.mkv` used to be impossible. The page
  now runs a three-rung ladder:
  1. `prebufferStream` fetches the head of the streamer's **`?raw=1`** stream (a
     real range read) and reads its `Content-Type` — that is how the page learns
     the true container without any new server-side DTO field.
  2. `needsClientRemux` sends anything that isn't `video/mp4|webm|ogg` to
     `attachClientRemux`, which lazily `import()`s `static/mkvplayer.js`. That
     module demuxes the container with **Mediabunny** (pinned CDN ESM import;
     ~650 KB, never fetched for an `.mp4`), remuxes the encoded packets — no
     re-encoding — into fragmented MP4, and appends them to a `MediaSource`.
     Seeking restarts the muxer at `getKeyPacket(t)`, so a scrub becomes an
     ordinary range request the streamer can prioritize.
  3. Codecs the browser cannot decode (AC3/E-AC3/DTS audio, HEVC without a
     hardware decoder) are caught up front by `canDecode()`, which throws
     `UnsupportedMediaError`; `fallbackToTranscode` then re-points the element at
     the plain (no `?raw=1`) URL — the old ffmpeg path. The fallback is
     **one-shot** per source so the two paths cannot ping-pong.

  The `<video>` `error` handler also inspects `video.error.code`:
  `MEDIA_ERR_SRC_NOT_SUPPORTED` (4) is a codec problem, not a transport one, so it
  goes straight to the fallback instead of the reload/re-prepare loop that would
  otherwise report it as a missing-seeder error.

  Because MSE only replaces the element's `src`, Plyr, the WebVTT `<track>`
  subtitles and the resume-position logic are all unaffected. Embedded ASS/SSA
  subtitle tracks inside the `.mkv` are now reachable in principle but are **not**
  surfaced yet.

  **`static/mkvplayer.js` is duplicated byte-for-byte into `admin/static/`** (where
  it is `//go:embed`ed) — the two services duplicate rather than share, as they
  already do for their query layers. Keep the two copies in sync. The admin wraps
  it in its own `static/mkvattach.js` glue, which decides by file **extension**
  rather than by sniffed `Content-Type`, because admin pages know the file path
  and this one does not.

- **Accounts & sessions** (`googleauth.go`, `session.go`, `auth.go`). Sign-in is
  Google OAuth2 authorization-code + OIDC, **hand-written** rather than via
  `golang.org/x/oauth2` — the same convention as every other external client in
  this repo (`manager.go`, admin's `tmdb.go`/`opensubtitles.go`). We never call a
  Google API after login, so no token is stored: the `id_token` is decoded once
  for its claims and discarded. Identity is `(provider, provider_uid)` — the
  Google `sub` claim — **not** the email, so a user who changes their Gmail
  address keeps their row and their saved list.
  - `decodeIDToken` deliberately **does not verify the signature**. That is
    correct *only* because `exchange` fetches the token itself, server-side, over
    verified TLS from `oauth2.googleapis.com` (OIDC Core 3.1.3.7 clause 6); we
    still check `aud`/`iss`/`exp`/`email_verified`. **If this is ever reused for
    a token arriving from the browser — Google One Tap, an implicit flow — it
    becomes a complete auth bypass.** See the comment on the function.
  - The session is a **signed cookie, not a table**: `v1:<userID>:<expiry>` plus
    an HMAC-SHA256 over `SESSION_SECRET`, 30 days, `HttpOnly` + `Path=/` +
    `Secure` (only when the public URL is https, or local dev would silently drop
    it). `SameSite=Lax` is **required, not a preference**: the OAuth callback is a
    top-level cross-site GET from `accounts.google.com`, and `Strict` would
    withhold the state cookie so every login would fail. `Lax` still keeps the
    cookie off cross-site POSTs, which is what protects the write API.
  - CSRF on the callback is a nonce in a 10-minute signed `phimnet_oauth` cookie,
    compared in constant time against the `state` parameter. The post-login
    return path rides in that **cookie**, not in `state`, so it never round-trips
    through Google. `safeNext` rejects anything not a plain site-relative path
    (`//evil.com`, absolute URLs, backslashes) — the open-redirect guard.
  - `currentUser` is applied **globally** so every page renders its own header
    state; it returns before touching the DB when there is no cookie, so
    anonymous traffic (and every crawler) pays nothing. A cookie that no longer
    resolves to a row is cleared. `requireUser` gates the write API only — read
    pages degrade to the anonymous rendering rather than redirecting.
  - Templates get the user through the `pageData` envelope (`server.go`):
    `render` executes `layout` with `{Data, User, LoginURL, SavedIDsJSON, Path}`,
    and `layout.html` re-scopes its three override blocks with
    `{{block "content" .Data}}`. **No page template had to change** — inside a
    `{{define "content"}}` the dot is still the handler's own view model. If you
    add a page, that is the only contract to honour.

- **Bookmarks / "xem sau"** (`static/bookmarks.js`, `templates/bookmarks.html`).
  The watch-later list runs in one of **two modes**, chosen from
  `<body data-bm-mode>` which `layout.html` renders per request:
  - **`local`** (anonymous) — unchanged from before accounts existed. The list
    lives entirely in `localStorage` under `phimnet.bookmarks` (same convention as
    the watch page's `phimnet.subStyle`): a newest-first JSON array, capped at
    500, of **full card snapshots** (`id`, `href`, `title`, `original`, `poster`
    URL, `year`, `type`, `score`, `vietsub`, `savedAt`). Storing the snapshot —
    not just the id — is what lets `/bookmarks` re-render the grid with **no
    server round trip and no new store query**.
  - **`server`** (signed in) — the list lives in `user_bookmarks`, so it follows
    the visitor across devices. Only ids are held client-side (the set the server
    renders onto `<body data-bm-ids>`, so every save button is correct on first
    paint with no extra fetch and no flash); the cards themselves come from the
    server-rendered `card` partial, which means a saved entry can never show
    stale metadata and a deleted title simply cascades out of every list. Toggles
    are optimistic and revert on a failed write; a card is only removed from the
    saved grid once the server confirms.
  - **The merge.** On any load where a signed-in visitor still has a localStorage
    list, `mergeLocal` POSTs its ids to `/api/bookmarks/merge` and drops the key
    *only after* the server accepts — so a failed merge simply retries next load.
    It is deliberately **not** gated on a "just logged in" marker, so it
    self-heals on every device. The server side is `INSERT IGNORE`, making it
    idempotent and letting ids that no longer exist fall away on the foreign key
    (a stale list is expected input, not an error).
  - Save buttons are rendered by the `card` partial and `detail.html` as
    `[data-bm-toggle]` elements carrying the snapshot in `data-bm-*` attributes;
    one delegated `click` listener on `document` handles all of them, including
    the cards the script builds itself. Because the `card` partial must hold a
    `<button>` (which may not nest in an `<a>`), `.card` is a `<div>` and the
    whole-card link is the stretched `.card-link::after` overlay — **keep
    `buildCard` in the JS in sync with that partial's classes** (it is still
    needed for local mode). Every storage access is `try/catch`-guarded so
    blocked storage degrades to "nothing saved".

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
  manager (with the internal bearer token): `addTorrent(magnet, torrentFile) →
  (infoHash, streamerPublicURL)` and `deleteTorrent(infoHash)` (used by the
  watch-session reaper above). `GetVideo` loads the source's stored `.torrent`
  bytes (`torrent_sources.torrent_file`) into `Video.TorrentFile` — the only load
  that does; the list queries skip the blob — and `handlePrepareSource` passes them
  along. When present, `addTorrent` sends a **multipart `.torrent` upload** (with
  the magnet alongside so the manager can still dedupe by infohash) so the streamer
  skips the slow DHT metadata fetch; otherwise a plain magnet. The admin writes and
  backfills those bytes (viewer is read-only). The manager picks a streamer on add;
  the returned public URL flows through the prepare response so the browser streams
  from the right instance. Keeping these on the server means only the streamers'
  stats + stream endpoints are browser-reachable; everything else is internal.

- **Subtitle blob store** (`blobstore.go`): a **read-only** port of admin's store
  (`Get` only, `local` + `s3`); `handleSubtitleFile` routes a subtitle row to
  `s.blobs[storage_backend]` and serves the bytes (`errBlobNotFound` → 404).

- **Domain types** (`models.go`): `Title` → `Genre`/`Season` → `Episode`, plus
  `Video` and `Subtitle` (mirroring admin; `Video.Magnet` is `json:"-"` so it is
  never serialized to the browser). Dates are `"YYYY-MM-DD"` strings. Deliberately
  a separate copy from admin's (no shared package).

- **Store** (`store.go`): `database/sql` queries — read-only for the catalog,
  read/write for `users` + `user_bookmarks` only (see the invariant at the top).
  `titleSummaryColumns` + `scanTitleSummary` are the shared SELECT list and
  scanner behind every card query (grid, browse rows, saved list) so they cannot
  drift; the columns are **qualified with the alias `t`**, so callers must select
  `FROM titles t` — `SavedTitles` joins `user_bookmarks`, which also has an `id`.
  `UpsertGoogleUser` relies on `id = LAST_INSERT_ID(id)` in its
  `ON DUPLICATE KEY UPDATE` so `LastInsertId()` returns the *existing* row's id
  on a returning login (without it you get 0) — works on both MySQL 8 and the
  MariaDB used in production.
  - `ListTitles(filter, limit, offset)` — one **page** of the discovery list
    (optional free-text title `LIKE`, genre, and type constraints), newest first.
    `CountTitles(filter)` returns the total for the same filter so the grid can
    render a numbered pager; both share `titleFilterClause` so the page and the
    count always agree.
  - `ListRows` — the browse home: loads every title once, then buckets into rows
    (movies, then TV, then one row per genre), keeping each row newest-first with a
    single pass over the title order. Empty rows are omitted; each row is capped at
    `rowLimit` (the carousel's heading links to the paginated grid for the rest).
    The leading **"Top 10 nổi bật hôm nay"** ranked strip is the admin's curated
    `FeaturedTitleIDs` (same list/order as the hero), falling back to a
    `vote_average` ranking when nothing is featured — so it and the hero always
    agree.
  - `ListGenres` — only genres attached to at least one title (filter dropdown).
  - `FeaturedTitleIDs(limit)` — the admin-curated hero picks, in `featured_titles.position`
    order (joined to `titles` so a since-deleted pick drops out); empty ⇒ the home
    hero falls back to score-ranked titles. The admin owns/writes this table.
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

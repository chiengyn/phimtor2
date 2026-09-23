# CLAUDE.md — viewer/

Guidance for the **public viewer** service
(`module github.com/chiengyn/phimtor2/viewer`). File paths below are relative to
`viewer/`. For the repo-wide picture and the other two services, see the root
[`../CLAUDE.md`](../CLAUDE.md).

## What this is

The **read side** of the shared catalog: a public, server-rendered browse /
discovery / watch UI over the movie/TV metadata that [`admin/`](../admin/CLAUDE.md)
imports. It renders localized Go `html/template` pages under explicit `/vi/...`,
`/en/...`, `/zh-cn/...`, `/zh-tw/...`, `/ko/...`, and `/ja/...` routes. `i18n.go` embeds `locales/*.json`; its
`localeDefinitions` registry drives supported routes, catalog loading, template
parsing, SEO locale metadata, and the header language dropdown. The dropdown
uses native `<details>` links and preserves the current page/query.
The browse/discovery flow is **fully server-rendered with no JS framework**:
filtering is a GET `<form>` and pagination is plain `<a>` links, so every state
has a real, shareable URL (search/genre/type/page all live in the query string). The only
JS is plain vanilla, no build step: the Plyr-based watch page's inline script,
small inline carousel helpers, `static/bookmarks.js` (loaded site-wide from
`layout.html`, see *Bookmarks* below), and `static/mkvplayer.js` (an ES module the
watch page `import()`s only when it meets a container the browser cannot demux —
see *Playing `.mkv`* below).

It **owns no schema and never runs migrations** —
[`admin/`](../admin/CLAUDE.md) is the sole owner, and the viewer assumes the
tables already exist. It is **read-only for the catalog** (`titles`, `videos`,
`featured_titles`, …) with exactly one exception, noted below.

The viewer writes **four** things, and nothing else, ever:

1. `users` (created/refreshed on login) and 2. `user_bookmarks` (the saved
list) — both declared by `admin/migrations/0007_users.sql`, both tables the
admin owns but never writes (bar the `comp_*` columns `0009` adds).
3. **`subtitles`, insert-only** — a signed-in visitor can search a provider
from the watch page and save the result for everyone (*Contributed subtitles*
below). Contributed rows carry `added_by_user_id`; admin-curated rows leave it
`NULL`. So this table has two writers separated by **rows**, where `users` has
two writers separated by **columns**.
4. `tv_devices` (paired televisions), declared by
`admin/migrations/0012_tv_devices.sql` — the same arrangement as `0007`: the
admin declares it and never writes it, because pairing is an account action and
accounts live entirely on this side. See *The TV client API* below.

> That third one is a deliberate hole in an otherwise airtight invariant, so do
> not "restore" it by accident. The reasoning is in
> `admin/migrations/0011_subtitle_contributor.sql`; the narrow blast radius is
> that the viewer only ever INSERTs there — it never updates or deletes a
> subtitle row, which stays an admin action.

> Deploy ordering matters because of this: the admin must apply `0007` **before**
> a viewer that writes those tables boots, `0011` before one that contributes
> subtitles, and `0012` before one that pairs televisions. The `0007` failure is
> contained by design — `currentUser` and the auth handlers treat a store error
> as "anonymous" and log it, so an un-migrated database breaks sign-in only, not
> the site.

## Commands

```bash
go build -o viewer .   # build (static CGO_ENABLED=0 binary)
go run .               # run (listens on :8082, needs the shared MySQL)
go vet ./...
go test ./...          # hermetic: no database, no network

# The device-pairing + TV-API integration tests need a real MySQL with the
# admin's migrations applied (docker compose up -d, then run the admin once).
# They are SKIPPED unless the DSN is set, so the line above stays hermetic.
PHIMTOR_TEST_DSN='phimtor:<pw>@tcp(127.0.0.1:3306)/phimtor?parseTime=true&charset=utf8mb4' \
  go test -run Integration ./...
```

Those integration tests exist because the things they check are owned by the
**database**, not by Go: the UNIQUE/NULL interaction that recycles a settled
`user_code`, the `status = 'pending'` guard that makes approval single-use, and
`expires_at <= NOW()` being evaluated by MySQL rather than compared against a Go
`time.Time`. `TestTVWatchLocksMatchResolutionLockIntegration` additionally pins
the central claim of the TV API — that its watch endpoint reports exactly what
`resolutionLock` computes — by asserting the two agree for the same access
snapshot. They clean up after themselves apart from one throwaway `users` row,
left deliberately (deleting it would cascade).

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
- Subtitle storage (`blobstore.go`): the viewer shares the *same* storage the
  admin writes to. `SUBTITLE_STORAGE_BACKEND` (`local`|`s3`),
  `SUBTITLE_STORAGE_DIR` (`./data/subtitles` — for `local` this **must** be the
  same directory admin writes to, a shared volume under compose, **and be
  writable**), and the same `S3_*` vars as admin (used with the same bucket).
  Mostly reads, but `Put` runs on the contributed-subtitle path; `Delete` exists
  for exactly one caller — rolling back a blob whose row insert failed.
  `SUBTITLE_STORAGE_BACKEND` finally does something here (it picks `blobPrimary`,
  where new writes land); reads still route by each row's own recorded backend.
- Subtitle providers (`OPENSUBTITLES_API_KEY`, `OPENSUBTITLES_USER_AGENT`,
  optional `OPENSUBTITLES_USERNAME`/`PASSWORD`, `SUBSOURCE_API_KEY`,
  `SUBSOURCE_USER_AGENT`; env-only secrets). These are the **same keys the admin
  uses** — one upstream account, one daily download quota — but here the quota is
  reachable by any signed-in visitor rather than one operator, hence
  `SUBTITLE_SEARCH_PER_HOUR` (30), `SUBTITLE_DOWNLOADS_PER_DAY` (10) and
  `SUBTITLE_DOWNLOADS_GLOBAL_PER_DAY` (150). **Both keys empty ⇒ the feature is
  off**: routes unregistered, no panel. `subtitleSearchEnabled()` also requires
  accounts, because the endpoints sit behind `requireUser` and the panel's
  anonymous state is a sign-in prompt that would otherwise link nowhere.

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

- **Routes** (`server.go` `setupRouter`): every human page exists under every
  registered locale prefix; `/` negotiates from the locale cookie / browser
  language, while old unprefixed page routes permanently redirect to Vietnamese.
  - `GET /{locale}/` — home, fully server-rendered. Supported locales are `vi`,
    `en`, `zh-cn`, `zh-tw`, `ko`, and `ja`. With an active filter
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
  - `GET /{locale}/titles/{id}` — full detail page (genres, and for TV its seasons/episodes).
  - `GET /{locale}/bookmarks` — the saved list, in **two modes** (see *Bookmarks*
    below). Anonymous: a static shell filled client-side from localStorage.
    Signed in: server-rendered from `user_bookmarks` with the same `card`
    partial the grid uses. `noindex` in both, and deliberately absent from
    `sitemap.xml`.
  - `GET /{locale}/watch/movie/{id}` and `GET /{locale}/watch/episode/{id}` — the watch page.
  - `POST /api/sources/{videoID}/prepare` — viewer-mediated playback (see below).
  - `GET /api/subtitles/{id}/file` — serves a saved subtitle file from the
    shared blob store, by the row's `storage_backend` + `storage_key`. **Stays
    anonymous**: reading a saved subtitle needs no account, only contributing one
    does. That is why the contribution routes are a `chi.Group` rather than a
    `chi.Route` on `/api/subtitles` — a subrouter would have swallowed this one.
  - `GET /api/subtitles/search`, `GET /api/subtitles/download`,
    `POST /api/subtitles` — the contributed-subtitle API, behind `requireUser`
    and registered only when a provider key is configured.
  - `GET /api/catalog/cards` — localized public card metadata used to refresh
    anonymous localStorage bookmarks after a language switch.
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
  can also load a local `.srt`/`.vtt`, or search a provider — see *Contributed
  subtitles* below.

- **Contributed subtitles** (`subtitleprovider.go`, `opensubtitles.go`,
  `subsource.go`, `ratelimit.go`, `subtitles_http.go`). A signed-in visitor can
  search OpenSubtitles/SubSource from the watch page. Two distinct actions per
  result, and the split is the whole design:
  - **clicking a result** → `GET /api/subtitles/download` returns the WebVTT and
    the page feeds it to the existing `setSubtitle()`. **Nothing is persisted**,
    so trying a subtitle is free and reversible; it is gone on reload.
  - **"save for everyone"** → `POST /api/subtitles` downloads it, writes the file
    to the shared blob store and inserts a `subtitles` row stamped with
    `added_by_user_id`. From then on **every** visitor sees it. This is the
    viewer's one catalog write.

  The panel renders for **everyone** when the feature is configured; anonymous
  visitors get it disabled (`data-locked`) with a sign-in prompt, the same call
  the quality chips make — a locked option is still listed because seeing that it
  exists is the point. Like those chips it is **client-rendered and bypassable**;
  `requireUser` on the API group is the real enforcement.

  Details worth keeping:
  - **The three provider files are duplicated from `admin/`, not shared**, same
    convention as `models.go`/`store.go`/`mkvplayer.js`. The port drops
    `stripASSOverrides` (already in `server.go`) and `parseSubtitleQuery` (the
    viewer seeds from the catalog row, never a file name). Fix one, mirror it.
  - **The search query is seeded with the ENGLISH title** (`SubtitleSearchName`,
    falling back to `original_title` then the row's own title). Providers index
    releases by their English/romanized name, so neither the localized title nor
    the original is reliably useful — a `/vi` page would seed "Câu Chuyện" and a
    Devanagari/Hangul original would seed `कहानी` / `기생충`, none of which match
    a release name. It is its own query because `GetTitle`/`GetEpisodeContext`
    join the page locale plus `fallbackLocale`, and that fallback is *Vietnamese*
    on an `/en` page, so neither reliably carries English. Only runs when the
    feature is enabled, so free traffic never pays for it. The seed is just a
    default in an editable box, so a lookup error falls back rather than failing
    the page.
  - `handleSaveSubtitle` does every cheap rejection — owner validation, dedupe,
    rate limit — **before** `provider.Download`, so a forged owner id or a
    duplicate can never spend a unit of the shared quota. The owner id comes from
    the browser, so it is checked against the catalog (`TitleExists`/
    `EpisodeExists`) rather than trusted; the FK would catch it too, but only
    after the download and as a 500.
  - A failed row insert **deletes the blob it just wrote**, or a shared volume
    accumulates files nothing references.
  - Errors never pass upstream `err.Error()` to the client (admin does, but it
    sits behind basic auth) — provider errors can echo the shared account's
    quota state.
  - Dedupe is in Go, not a UNIQUE index, and that is deliberate: MySQL treats
    NULLs as DISTINCT in unique indexes and every row has `title_id` or
    `episode_id` NULL, so a unique key over the owner tuple would never collide
    for **either** owner kind. See `FindSubtitleByProviderFile`.
  - **Known gap, shared with admin: no charset detection anywhere.** A
    Windows-1258/CP1252 Vietnamese `.srt` is stored and served as mojibake.
    OpenSubtitles is asked for WebVTT (UTF-8) so it is largely unaffected;
    SubSource ZIPs are the real exposure. Fix belongs in both services at once.

- **Quality tiers** (`server.go`, `resolutionLock`). Sources are always *listed*
  — seeing that a better quality exists is the whole point — but not always
  playable. Three tiers, from one predicate:

  | | anonymous | signed in, unpaid | entitled |
  |---|---|---|---|
  | 720p | plays | plays | plays |
  | 1080p (`memberResolutions`) | 🔒 sign-in chip | plays | plays |
  | 2160p (`lockedResolutions`) | 🔒 sign-in chip | 🔒 "Nâng cấp" | plays |
  | **auto-plays** | 720p | 1080p | 1080p |

  "Entitled" is any of three: a paid **time pass** (`users.plan_expires_at`), an
  **admin-granted comp** (`users.comp_expires_at`, set from the admin's `/users`
  page), or a **permanent per-title unlock** (`user_title_unlocks`).

  1080p is gated to push registration; 4K is the **paid** tier. 4K shows a
  sign-in chip rather than an upgrade chip to an anonymous visitor because
  entitlements hang off an account — signing in is step one of paying.
  `resolutionLock` returns `lockNone`/`lockMember`/`lockUpgrade`/`lockPaid` and
  is the **single source of truth** for the chip copy, for which sources are
  eligible to auto-play, and for the enforcement. Which *eligible* source
  actually auto-starts is a separate, purely presentational choice made in the
  page — see the auto-play bullet below.
  - **With billing unconfigured, 4K collapses back to "sắp ra mắt" for everyone
    who holds no entitlement** (`lockPaid`) — the pre-billing behaviour, and the
    billing rollback. An upgrade chip pointing at a localized `/plans` route that is not
    registered would be worse than no offer at all.
  - **Order matters in `resolutionLock`: entitlement is checked BEFORE
    `s.billing.enabled()`.** It was the other way round until admin comps
    arrived, and that was a bug — turning billing off revoked permanent title
    unlocks people had already paid for, and would have voided comps too.
    Billing-off means "you cannot buy", never "what you hold is void".
  - Entitlement is `unlimited || unlocked`: `unlimited` is every-4K access from
    either a paid pass or an admin comp (`HasPass` / `HasComp`, both read for
    free off the user the session middleware already loaded), and `unlocked` is
    the per-title purchase. `accessForTitle` builds that snapshot and is
    deliberately lazy — it skips the `user_title_unlocks` query entirely unless a
    paid-gated source is on the page (`hasLockedResolution`), so free 720p
    traffic pays nothing for a feature it cannot use. It is **not** gated on
    `billing.enabled()`, for the reason above.
  - A comp is written by the **admin**, into its own `users.comp_*` columns. The
    viewer must never write them, exactly as the admin must never write
    `plan`/`plan_expires_at` — disjoint columns are what make the shared row
    safe (see the root `CLAUDE.md`).
  - **The chips are client-rendered and bypassable — `handlePrepareSource` is
    the only thing that actually enforces this.** It answers `401` for a member
    lock, `402` for an upgrade lock and `403` for a paid one; the page turns the
    `401` into the sign-in gate and the `402` into the upgrade gate, which is also
    how a session expiring (or a pass lapsing) mid-watch recovers. Entitlements
    are per **title** but this endpoint is handed only a video id, and
    `videos.title_id` is NULL for an episode — hence `TitleIDForVideo`, which
    follows episode → season → title.
  - **4K never auto-plays; it is opt-in even when entitled.** The page's
    `pickDefaultVideo` (`templates/watch.html`) filters to the sources this
    visitor may play and walks `AUTO_RESOLUTIONS = ['1080p', '720p']`, so
    everyone signed in lands on 1080p and an anonymous visitor on 720p. 2160p is
    deliberately off that ladder — it is the heaviest source on the page and the
    slowest to buffer, and auto-starting it gave paying users the worst first
    second of playback. It is reached only by clicking its chip, or automatically
    when it is the *only* thing this visitor can play (a 4K-only title). This
    replaced "newest playable source", which is why an entitled visitor used to
    land on 4K: the store returns videos newest-first and nothing was locked for
    them. Recency still breaks ties *within* a quality.
  - **A manual pick sticks** in `localStorage` under `phimnet.quality` (same
    convention as `phimnet.subStyle`), so choosing 4K carries to the next
    episode. Only `chooseSource` — the chip click — writes it; `playSource` must
    not, or auto-play would overwrite the preference and one title with no 4K
    source would silently demote the visitor. The value is a **convenience, never
    an entitlement**: it can only ever select a source `resolutionLock` already
    marked playable, and a preference that no longer applies (a lapsed pass, a
    sign-out) simply misses the filter and falls back down the ladder.
  - **What the paywall actually protects is discovery, not bytes.** The gate
    withholds `{infoHash, streamerPublicURL}`, but the streamer's stream endpoint
    is unauthenticated with `Access-Control-Allow-Origin: *`, so a URL obtained
    once works for anyone until the torrent is reaped. This was acceptable when
    1080p was a registration nudge; it is a real (accepted, tracked) hole now that
    money is involved. The fix is short-lived HMAC-signed stream URLs verified by
    the streamer, which is a separate change spanning all four services.

- **Billing** (`billing.go`, `billing_http.go`, `chains.go`, `chain_evm.go`).
  Crypto only, watched in-repo — no BTCPay, no hosted processor, no webhook.

  - **An invoice is identified by a unique exact AMOUNT, not by an address.**
    There is one receive address per chain family and the price is nudged by a few
    units of dust; `UNIQUE(lock_ns, amount_lock)` in `0008` arbitrates so no two
    open invoices can claim the same amount. This is why **no key material — not a
    seed, not even an xpub — exists anywhere in this service**: nothing needs to
    derive or sweep an address. It also means one `0x` address serves *every* EVM
    chain, so funds land in one wallet with no per-address sweeping and no gas
    spent collecting dust. Preserve that property.
  - `amountDecimals` is **6**, the narrowest precision among the tokens watched
    (USDT/USDC are 6; **BSC-USD is 18**). An amount at 6 decimals is exactly
    representable on all of them, which is what lets one amount identify an
    invoice across chains that disagree about decimals. Amounts are decimal
    **strings** and `*big.Int`, never `float64` — they are compared for exact
    equality against `DECIMAL(36,18)`, and that comparison decides who gets paid.
  - **All open EVM invoices share the single `evm` lock namespace**, deliberately:
    no two can collide on *any* EVM chain, so the buyer may pay on whichever chain
    is cheapest and still be credited. `paid_chain` records where it landed, which
    may differ from the `chain` the invoice suggested.
  - **There is no under-payment tolerance, and there cannot be.** The amount *is*
    the identity, so a payment of the wrong amount does not under-pay an invoice —
    it matches none, and is logged as `UNMATCHED` for the operator to resolve. Do
    not add a fuzzy match here: it would let one transfer satisfy the wrong
    invoice.
  - **`chain_evm.go` is written once and instantiated N times.** A chain is a row
    in `evmChainDefs`, not code — adding Optimism is a row plus an entry in
    `BILLING_EVM_CHAINS`. **The contract addresses in that table are the security
    boundary**: scans filter by contract, so a scam token calling itself "USDT"
    can never credit an invoice unless its address is listed. Verify an address
    against a block explorer before enabling that chain.
  - Confirmations are enforced by **never looking at blocks that are not deep
    enough** (the scan ceiling is `latest - MinConf`), which handles reorgs for
    free instead of tracking and re-checking a pending set. Only ERC-20
    stablecoins are watched: a native ETH/BNB transfer emits no log, so
    `eth_getLogs` cannot see it.
  - **A chain can look healthy while crediting nothing.** The cursor advances off
    `eth_blockNumber`, which free RPC endpoints serve happily even when they
    refuse `eth_getLogs` — publicnode answers Arbitrum log queries with "Archive
    requests require a personal token" beyond ~20 blocks of the tip, and Arbitrum
    mints blocks ~4x a second, so every scan is "archive". This shipped to
    production on 2026-09-12 and was caught only by reading the container log.
    **When adding or repointing a chain, test `eth_getLogs` at `evmScanChunk`
    width, not just `eth_blockNumber`**, and watch for `billing: scan <chain>:`
    lines afterwards. A stuck `billing_chain_cursors` row is the other tell —
    the cursor is deliberately not advanced past a failed read, so nothing is
    silently skipped, but nothing is credited either.
  - **Idempotency lives in one place**: `SettleInvoice`'s `AND status = 'pending'`
    guard. A repeated observation, a restart mid-settle and a rewound cursor all
    collapse to zero rows affected. Do not "simplify" it away.
  - Every timestamp comparison uses MySQL's own `NOW()`, never a Go `time.Time` —
    the app and the database can disagree about clock or zone, and an invoice must
    not expire early because of skew.
  - Config: `BILLING_EVM_ADDRESS` + `BILLING_EVM_CHAINS` enable it (both needed);
    `BILLING_EVM_RPC_<CHAIN>` overrides a public endpoint; plus poll interval,
    invoice TTL, prices in US cents, and a display-only VND rate. **Billing also
    requires accounts** — an entitlement hangs off a user row. Unset the address
    and the whole tier goes inert, which is the rollback.
  - The QR encodes the **plain address**, not an EIP-681 transfer URI: an invoice
    accepts any enabled chain and any listed token, so baking one chain+token into
    the QR would contradict that, and a wallet mis-parsing the amount is
    unrecoverable when the amount is the identity.
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
  3. `MediaSource.isTypeSupported` checks the MP4 codec combination. This is
     a packet remux, so it does not require WebCodecs `canDecode()` support.
     Rejected codecs, asynchronous remux/SourceBuffer failures, and video decode
     errors trigger **one** fallback to `?transcode=1` (H.264 + stereo AAC),
     including native containers whose codecs the browser cannot decode.

  Aborting a source switch disposes pending probes and active playback. The pump
  pauses for queued bytes as well as buffered duration, retries quota failures
  as playback advances, preserves delayed-start audio and the latest seek target,
  and signals end-of-stream after the final append. The compatibility fallback
  uses server CPU and remains sequential/unseekable; it does not promise support
  for input containers that require backward reads through FFmpeg's stdin pipe.

  The `<video>` `error` handler also inspects `video.error.code`:
  `MEDIA_ERR_DECODE` (3) and `MEDIA_ERR_SRC_NOT_SUPPORTED` (4) trigger compatibility mode, so the player
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
    URL, `year`, `type`, `score`, subtitle state, `savedAt`). Anonymous bookmark
    pages refresh those snapshots through the localized public card endpoint, so
    switching languages cannot leave stale copy. Keeping full snapshots still
    provides an immediate offline/failure fallback while that refresh runs.
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

- **The TV client API** (`tvapi.go`, `device.go`, `templates/link.html`). The
  Android TV app is a second **client** of this service, never a second
  implementation of it — the whole design follows from that.
  - `/api/tv/v1/*`. **The only versioned path in the repo**, because it is the
    only client that cannot be force-updated: an APK installed today will still
    be calling these URLs in two years. Wire types are declared in `tvapi.go`
    rather than reusing `models.go` structs, for the same reason — renaming a
    field must not silently break a client nobody can push a fix to. The two
    exceptions are `watchVideo` / `watchSubtitle`, reused as-is because they are
    already a published contract.
  - **The watch endpoints emit `s.toWatchVideos(videos, access)` verbatim**, so
    `resolutionLock` stays the single source of truth across three clients
    instead of two, and `handlePrepareSource` remains the only enforcement —
    the TV's chips are advisory exactly as the web page's are. `watchSubtitle`
    gained a `format` field for this: the browser hands the URL to a `<track>`
    and lets the element work it out, but a native player must be told
    `text/vtt` vs `application/x-subrip` before it opens the file.
  - Locale rides in `X-Phimnet-Locale` (the existing JSON convention) or
    `?locale=`. Row labels are **resolved server-side**, so a locale the app
    ships no strings for still reads correctly.
  - **Read routes are always registered; pairing routes need accounts.** With
    `GOOGLE_CLIENT_ID=""` the app browses anonymously at 720p and offers no
    sign-in, which is the documented rollback — pinned by
    `TestTVRouteRegistration`.
  - **Pairing is device-code (RFC 8628 shapes), not OAuth.** The TV shows a
    short code, the user approves it at `/{locale}/link` on a phone that is
    already signed in, and the TV trades its long `device_code` for a bearer
    token. No Google token ever reaches the television — which is the point:
    `decodeIDToken` skips signature verification (sound only because `exchange`
    fetches the token server-side over TLS), so any flow letting a device
    present us a Google token would turn that shortcut into a **complete auth
    bypass**. See the comment on the function.
  - **Two codes, two jobs.** `device_code` is long, secret and crypto-random —
    the actual credential, seen only by the TV. `user_code` is 8 characters
    read off a screen across a room, so its alphabet drops every ambiguous
    glyph (`O/0`, `I/1/L`, `S/5`, `B/8`, `U/V`) and it is necessarily
    low-entropy: the 15-minute expiry, single use, and the per-account rate
    limit are what make that safe, not the code itself.
  - `user_code` is `UNIQUE` but **NULLable, and NULLed the moment a pairing
    settles**. MySQL treats NULLs as DISTINCT in a unique index — the same
    property `0011` works around — so no two *open* pairings can collide while
    a settled one stops burning a code out of a small alphabet forever. A
    collision on insert is an ordinary outcome, retried via `isDuplicateKey`,
    exactly as the billing amount reservation does.
  - **The bearer token is the session cookie's construction with a different
    payload version** (`v1tv:<userID>:<deviceID>:<exp>` vs `v1:<userID>:<exp>`),
    and each reader accepts only its own. That separation is the *only* thing
    stopping a stolen cookie being replayed as a TV token, so
    `TestDeviceTokenAndSessionCookieAreNotInterchangeable` asserts it in both
    directions. `currentUser` tries the cookie, then the bearer.
  - **Why `tv_devices` exists at all**, when a session is famously a signed
    cookie with no storage behind it: rotating `SESSION_SECRET` is a stateless
    token's only revocation lever, and that is the right trade for a browser
    somebody controls and the wrong one for a television in a shared room. The
    row makes "sign out this TV" one `UPDATE`, at the cost of one indexed lookup
    per TV request (`DeviceActive`, on TV traffic only). Token TTL is 180 days
    because a TV is paired once — the row, not the expiry, is the lever.
  - Abandoned pending pairings are swept every 10 minutes by
    `reapDevicePairings`, started from `main.go` and a no-op without accounts.

- **Subtitle blob store** (`blobstore.go`): a full port of admin's store
  (`Put`/`Get`/`Delete`, `local` + `s3`); `handleSubtitleFile` routes a subtitle
  row to `s.blobs[storage_backend]` and serves the bytes (`errBlobNotFound` →
  404), while a contributed subtitle is written to `s.blobPrimary`.
  `subtitleStorageKey` is ported verbatim so both services produce identical key
  layouts in the shared store, and its nanosecond suffix is what stops a viewer
  save ever clobbering an admin one.

- **Domain types** (`models.go`): `Title` → `Genre`/`Season` → `Episode`, plus
  `Video` and `Subtitle` (mirroring admin; `Video.Magnet` is `json:"-"` so it is
  never serialized to the browser). Dates are `"YYYY-MM-DD"` strings. Deliberately
  a separate copy from admin's (no shared package).

- **Store** (`store.go`): `database/sql` queries — read-only for the catalog
  except `AddSubtitle`, plus read/write for `users` + `user_bookmarks` (see the
  invariant at the top). `AddSubtitle` also calls `refreshTitleVietsub`, which
  UPDATEs `titles.has_vietsub`. That second catalog write is deliberate: the flag
  is not content but a cache derived from the subtitle rows just written, and
  **nothing recomputes it on a schedule**, so skipping it would mean a
  contributed Vietnamese subtitle never lights the "Vietsub" badge or reaches the
  subtitled browse row — the exact signal the feature exists to improve.
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

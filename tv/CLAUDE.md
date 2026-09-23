# CLAUDE.md — tv/

Guidance for the **Android TV / Google TV client** (`online.phimnet.tv`). File
paths below are relative to `tv/`. For the services it talks to, see the root
[`../CLAUDE.md`](../CLAUDE.md) and [`../viewer/CLAUDE.md`](../viewer/CLAUDE.md)
(*The TV client API*).

## What this is

A native Kotlin / Compose-for-TV / Media3 app that is a **second client of the
viewer, never a second implementation of it**. Everything that decides what a
person may watch stays on the server: the app renders the `lock` the viewer's
`resolutionLock` computed, and `POST /api/sources/{id}/prepare` — the same
endpoint the web watch page calls — is still the only enforcement. The app has
no tier logic to get wrong, and must not grow any.

Why native at all: the catalog is mostly `.mkv`, which browsers cannot demux.
The web page copes with a remux-in-JavaScript ladder and an unseekable ffmpeg
fallback; **ExoPlayer demuxes Matroska, AVI and MPEG-TS natively**, so the TV
plays the torrent's own bytes with real range seeking and hardware decode.

It is **sideloaded, never published to a store**: people install it with
Downloader (or any sideloader) from `<viewer-host>/tv`, which the viewer serves.
See *Releasing* below; Kamal is not involved.

## Commands

```bash
export JAVA_HOME=<a JDK 21>          # AGP does not run on the newest JDKs (27 fails)
export ANDROID_HOME=~/Android/Sdk    # platforms 37 + 36, build-tools, platform-tools

./gradlew :app:testDebugUnitTest     # JVM unit tests (no device)
./gradlew :app:lintDebug             # keep this at zero findings
./gradlew :app:assembleDebug         # → app/build/outputs/apk/debug/
./gradlew :app:assembleMinified      # R8 build that can still reach a local viewer
./gradlew :app:assembleRelease       # unsigned unless PHIMNET_TV_KEYSTORE* is set
python3 tools/sync_strings.py        # regenerate res/values*/strings.xml
```

CI is `.github/workflows/android.yml`: tests + lint + a minified release on every
change under `tv/`; a `tv-v*` tag also signs and **publishes** it (see
*Releasing*). Never `v*`, which is the Docker trigger for the Go services.

### Build types

| | server default | cleartext | minified | notes |
|---|---|---|---|---|
| `debug` | `http://10.0.2.2:8082` | yes | no | `10.0.2.2` is the emulator's alias for the host, where `go run .` in `viewer/` listens |
| `minified` | same as debug | yes | **yes** | release's R8 config, debug signing. **Smoke-test this before shipping** — R8 breaking serialization or type-safe navigation only shows at runtime |
| `release` | `https://phimnet.online` | **no** | yes | the bearer token rides on every request, so http is refused, in the network config *and* in Settings |

The server is a **setting**, not a constant — phimtor2 is self-hosted. Changing it
signs the TV out (a token only means something to the server that minted it).

### Toolchain decisions (each one was forced; do not "tidy" them)

- **AGP 9 compiles Kotlin itself** ("built-in Kotlin"). Applying
  `org.jetbrains.kotlin.android` is a hard error. The Compose and serialization
  compiler plugins are still applied, and their version pins the Kotlin compiler.
- **`compileSdk = 37`, `targetSdk = 36`.** Current Compose / Navigation / Coil AAR
  metadata refuses to compile against less than 37. `targetSdk` is what opts into
  runtime behaviour changes and stays on 16 until Android 17 has been tested
  (suppressed in `app/lint.xml` with that reason).
- **`minSdk = 24`** — Navigation 2.10 is the floor. Cheap TV boxes run old Android.
- **No `FilterChip`**: tv-material 1.1 still marks it experimental. `ChoiceChip`
  (`ui/common/ChoiceChip.kt`) is built on the stable selectable `Surface`.
- **No icon library**: three vector drawables (`ic_play`, `ic_pause`, `ic_lock`).
- The Gradle wrapper pins `distributionSha256Sum`.

## Architecture

Single `:app` module, hand-wired (`AppGraph.kt` — six objects; no DI framework,
matching the repo's preference for small explicit clients).

- `api/` — `PhimnetApi` (viewer), `StreamerClient` (stats), `Dto.kt`.
- `auth/Pairing.kt` — the device-code flow as a plain `Flow`.
- `playback/` — the load-bearing logic, **kept free of Android types** so it is
  unit-tested on the JVM: `StreamUrls`, `QualityPicker`, `awaitStreamReady`,
  `WatchSession`, `RecoveryPolicy`, `SubtitleMime`; plus `PlayerViewModel`, which
  wires them to ExoPlayer.
- `settings/` — DataStore prefs, `ServerUrl` validation, `SiteLocale`.
- `update/` — the in-app updater (see *In-app updates*): `UpdatePolicy` and
  `ApkDownloader` are JVM-tested; `Updater` and `InstallStatusReceiver` drive
  Android's `PackageInstaller`.
- `ui/` — one package per screen (home, browse, detail, player, link, settings)
  and `ui/common/` (cards, top bar, QR, `Load`, focus helper, `ChoiceChip`).

## Invariants — each of these has broken, or would silently

- **Stream with `?raw=1`. Always.** Without it the streamer sees any container
  that is not mp4/webm/ogg — nearly every `.mkv` — and pipes it through ffmpeg:
  it *plays*, but can never seek, and costs a streamer CPU core per viewer.
  `StreamUrls` owns this and `StreamUrlsTest` pins it. `?transcode=1` is used only
  as the fallback below, never alongside `raw=1` (the streamer lets `raw` win).
- **Heartbeat from the moment `prepare` returns** (`WatchSession`, every 10 s), and
  **leave** on stop, background (`ON_STOP`) and end of playback. The viewer drops a
  torrent after `WATCH_HEARTBEAT_TTL` = **30 s** of silence — mid-playback, not as
  tidy-up. The beat starts *before* the readiness wait because metadata can take
  longer than the TTL to arrive. Coming back to the foreground prepares afresh
  and resumes at the saved position.
- **Readiness is `totalBytes > 0`, never `bytesCompleted > 0`.** In the streamer's
  download-all mode nothing downloads until a reader opens the file; waiting for
  bytes deadlocks. Poll 1 s, give up at 60 s (`NoPeers`), as the web page does.
- **The viewer's token never reaches a streamer.** It is attached per request in
  `PhimnetApi`, not by an OkHttp interceptor, because the same pool feeds
  ExoPlayer and the stats poll, which go to other hosts. `streamHttp` is the
  streamer client (longer read timeout: a swarm pauses between pieces).
- **Every suspend function in `api/` is main-safe** (`withContext(IO)`). `await()`
  resumes on the *caller's* dispatcher — Compose's main thread — and reading the
  body there is `NetworkOnMainThreadException`. JVM tests cannot see this (no
  main-thread policy); it was found on the emulator. `Load` logs the real cause
  and `errorText` only says "can't reach the server" for a genuine `IOException`.
- **Wire compatibility is the app's half of API versioning.** `WireJson` has
  `ignoreUnknownKeys` + `coerceInputValues`; every optional field has a default.
  DTOs mirror `viewer/tvapi.go` byte for byte. The two camelCase shapes
  (`prepare`'s answer, the heartbeat body) are the web page's existing contracts.
- **Recovery ladder** (`RecoveryPolicy`): a stream `404` means the torrent was
  reaped → prepare again (≤ 3, budget reset on `STATE_READY`); a decoder / format
  error, or a file whose every audio or video track is unsupported → **one**
  fallback to `?transcode=1` (it cannot seek, so it starts at 0 and says so).
- **Do not read the streamer's speed fields.** It derives them from one sample slot
  per infohash shared by every caller, so a TV and a browser on the same torrent
  corrupt each other's numbers. Peer counts and byte counts are live and fine.
- **Quality**: `QualityPicker` is a port of the web page's `pickDefaultVideo` —
  1080p → 720p, **4K never auto-plays**. Only a *manual* pick writes the
  remembered quality (`phimnet.quality`); automatic playback must not, or one title
  without 4K would silently demote the person everywhere after it.
- **Gates**: `lock` from the server picks the copy — `member` → pair this TV,
  `upgrade` → QR to the viewer's `/plans?title=`, `paid` → coming soon. A locked
  chip picked **mid-film** raises the gate as a dialog over the *paused* film:
  Back returns to it, and pairing from it resumes at the same position
  (`handoffPosition`).
- **Tracks**: subtitle and audio pickers are built from ExoPlayer's real tracks, so
  **tracks embedded in the `.mkv` appear alongside saved subtitles** — something
  the web page cannot do yet. The first saved subtitle is applied once per source,
  as on the web; `format` (`srt`/`vtt`) picks its MIME.

### Remote control and focus

- **Something must always be focused.** A screen with nothing focused is a dead
  end on a remote. Found three times on the emulator: a panel appearing, a
  focused button being replaced (sign in ⇄ sign out), and a new tab screen.
  Rules:
  - initial focus goes through `FocusRequester.focusWhenReady()` (retries per
    frame until `requestFocus(Enter)` reports success) — never a one-shot
    `requestFocus()` in the first frame;
  - tab screens pass `focusCurrent = true` to `TopBar`, so left/right keeps
    walking the tabs (Home instead focuses its hero);
  - when a focused control is swapped for another, move focus to the new one
    (see the account button in `SettingsScreen`, and `UpdatePanel`, whose
    buttons change with every step once someone has pressed one — and Home,
    which puts focus back on the hero when "Later" removes the banner);
  - single-line text fields take `dpadLeavesTextField()`: `BasicTextField`
    swallows ↑/↓ as cursor moves, so with the keyboard closed the remote could
    not get out of the server field to the controls below it.
- Player: with controls **hidden**, the D-pad drives playback (centre =
  play/pause, ←/→ = 10 s, the web page's `SEEK_STEP`; ↑/↓ = show controls); with
  controls **shown** it moves focus, and Back hides them. Media keys always work.
  Controls auto-hide after 5 s while playing, never while paused. `PlayerView`
  must stay non-focusable or it steals the remote.
- Returning from a title restores focus to the card that was opened.

### Pairing (`auth/Pairing.kt`, `ui/link/`)

RFC 8628 as the viewer implements it: show the short code and a QR of
`verification_uri_complete` (the phone lands on `/link` pre-filled), poll at the
server's `interval`, `slow_down` adds 5 s, an expired code is replaced silently.
After **`MAX_CODES` (4) unused codes — an hour — it stops** (`Idle`) and offers a
button: left on for three hours during testing it polled 2,217 times. The TV is
named after `Settings.Global` `device_name` in the account's device list.
Sign-out calls `device/logout` first, so the token is revoked, not just forgotten.

### Storage and privacy

DataStore holds the server, token, content-language override, remembered quality
and resume positions. `allowBackup="false"` **and** `dataExtractionRules`
exclude everything from cloud backup *and* device-to-device transfer — from
Android 12 the former alone no longer stops device transfer, and a restored copy
would carry a live credential onto someone else's TV.

### Strings

Six locales, like the site: `values` (en), `values-vi`, `values-b+zh+Hans`,
`values-b+zh+Hant` (split by **script**, so zh-HK/zh-MO get Traditional — the
same rule as `SiteLocale` and the site's `parseLocale`), `values-ko`, `values-ja`.
They are **generated** by `tools/sync_strings.py`, which reuses the site's own
translations from `viewer/locales/*.json` wherever the site already says it and
refuses to write if any locale is missing a string. Edit the script, never the XML.
Server-authored text (row labels, lock messages, episode captions) arrives
already translated via `X-Phimnet-Locale`. Settings' language picker changes the
**content** language; the app chrome follows the system (and Android 13+'s
per-app language, via `localeConfig`).

## Releasing (sideloaded)

`git tag tv-v1.2.3 && git push origin tv-v1.2.3`. The workflow stamps
`versionName` 1.2.3 and `versionCode` 1002003 (`major*1000000 + minor*1000 +
patch`; `app/build.gradle.kts` reads `PHIMNET_TV_VERSION_*`, and local builds get
1), signs with the release key from repo secrets, verifies the signature, copies
the APK to the host's `/srv/phimnet-tv`, switches `latest.json` atomically, and
then downloads it back from the live viewer to check the bytes. One-time setup
(the key, the secrets, one viewer deploy) is in `../DEPLOY.md` §7.

Why each piece is the way it is — all verified on the emulator:
- **The signing key is permanent.** A TV refuses an update signed by a
  different key; the only way out is uninstalling, which also unpairs it. A
  release tag without the key FAILS rather than publishing an unsigned APK,
  because Android will not install one at all.
- **`versionCode` only goes up.** An update over an installed release works in
  place (0.1.0 → 0.1.1: `Success`); going back is refused
  (`INSTALL_FAILED_VERSION_DOWNGRADE`). So pointing `latest.json` at an older
  APK only helps TVs that have not updated yet — bad releases are fixed forward.
- The prune step keeps the five newest APKs. It is a `while read` loop, not
  `grep -v | xargs rm`: with fewer than six APKs, grep exits 1 and `pipefail`
  failed the job *after* `latest.json` had already switched (caught in rehearsal).
- The APK served at `/tv` was fetched over HTTP and installed on the Android TV
  16 emulator: it installs, appears in the TV launcher, and upgrades.

### In-app updates (`update/`)

A sideloaded app has no store to update it, so it updates itself from the
viewer's `/api/tv/v1/app` manifest (version, SHA-256, size, absolute download URL):

1. **Check** once per launch, after settings load (before that, requests go to
   the default server), silently — a TV briefly offline hears nothing. Settings
   has a manual check that reports every outcome. `UpdatePolicy` offers a release
   only if its `versionCode` is **strictly higher** (Android refuses anything
   else), the download URL is on the **same origin** as the configured server,
   and the size and SHA-256 are usable.
2. **Offer** on Home as a banner pinned under the top bar — never a list item
   (focusing the hero scrolled it off-screen) and never the initial focus. The
   banner arrives after Home has laid out and shrinks the list, so Home resets
   the scroll once the pivot scroll settles, or the hero's title ends up under
   the banner. "Later" lasts until the next launch.
3. **Permission.** Android 8+ asks per app to "install unknown apps"
   (`REQUEST_INSTALL_PACKAGES` in the manifest does not grant it).
   `ACTION_MANAGE_UNKNOWN_APP_SOURCES` opens the TV's screen for it (exists on
   Android TV as `ExternalSourcesActivity`); returning to the app with it granted
   carries on by itself (`onAppResumed`). If a TV has no such screen, the panel
   points at reinstalling from `/tv` instead.
4. **Download** (`ApkDownloader`) into `cacheDir/updates` via a `.part` file,
   hashing as it goes; aborts the moment the body exceeds the promised size;
   renames into place only when size and SHA-256 both match. Old APKs are cleared
   on every launch.
5. **Install** through a `PackageInstaller` session (no `FileProvider` needed:
   the session takes a copy). `STATUS_PENDING_USER_ACTION` → start the system's
   confirm screen; `STATUS_FAILURE_ABORTED` (Cancel) → back to the offer, no
   error. Android itself refuses an update signed with another key — the
   guarantee the rest sits behind.
6. **Android kills the app to replace it and does not restart it**, and on
   Android 10+ the new version cannot restart itself either: both a
   `MY_PACKAGE_REPLACED` receiver and an activity `PendingIntent` as the session
   status were tried and blocked as background activity launches. So the
   "Installing" message says up front that the app will close and to reopen it
   from the home screen.

## Testing

- **Unit tests** (`app/src/test`, 64): stream URLs, quality ladder, readiness,
  heartbeat timing, recovery classification, pairing flow (virtual time), server
  URL and locale parsing, the update policy and the verifying APK download, and
  the API client against MockWebServer.
- **End to end on an emulator**, against a real viewer and database, with the
  harness in `e2e/` standing in for the manager and streamer:
  - `e2e/fake/` — a fake manager (`:18083`) + streamer (`:18090`) honouring the
    streamer's public contract: stats 404 until "metadata", `?raw=1` served with
    real Range support, `?transcode=1` sequential, `HEAD` → 405. **A stream request
    without `raw=1` is logged loudly and refused** — the bug it exists to catch.
    `THROTTLE_KBPS=300` makes it behave like a swarm rather than a LAN; without
    it the player buffers the whole file at once and no seek ever needs a new
    range, which proves nothing.
  - `e2e/media/make-media.sh` — generates the test files (H.264 + two AAC tracks +
    an embedded SRT with a burned-in clock; the same with AC-3 audio; the
    transcode's output).
  - `e2e/seed/` — adds / removes the test rows in the **local** database
    (`seed <dsn> <subtitle-dir> seed|cleanup|user|comp|uncomp`).

  Run the viewer from `viewer/` with `MANAGER_INTERNAL_URL=http://127.0.0.1:18083`,
  accounts on (any `GOOGLE_CLIENT_*`), and billing on with polling off
  (`BILLING_POLL_INTERVAL_SEC=0`) — then drive the emulator with
  `adb shell input keyevent` and read the fake's log. To approve a pairing, mint
  a `phimnet_session` cookie with the viewer's `SESSION_SECRET`
  (`v1:<userID>:<exp>` + HMAC-SHA256) and POST `/api/tv/v1/device/approve`.
  Headless: `emulator -avd <tv-avd> -no-window -no-audio -gpu swiftshader_indirect`.
  Do **not** use the Android CLI's `layout` command on a Google-services image:
  Play Protect blocks the helper APK it installs.

### Verified end to end (Android TV 16 emulator)

Browse; pairing (including three hours of code rotation and a lower-case,
space-separated code); anonymous 720p with the 1080p member gate; 1080p after
pairing; the 4K upgrade gate and its QR; 4K after an admin comp, falling back to
the transcode because the emulator has no AC-3 decoder; `raw=1` on every direct
stream, the Matroska index read near EOF, and **seeks past the buffer issuing new
open-ended ranges at the right clusters** — the shape the streamer's
`PrioritizeSeek` keys on; heartbeat every 10 s, leave → `DROP` on end and on
Home, re-prepare and resume on return; embedded audio/subtitle tracks; the saved
Vietnamese `.srt`; sign-out revoking the pairing; and the R8 (`minified`) build.

The in-app update, on the `minified` build against a local viewer serving a
published `latest.json`: banner on Home with the hero intact; Update → the
permission step → the TV's own settings screen → denied (stays on the step) and
granted (carries on by itself) → download → the system's confirm screen → Cancel
(back to the offer, focus on Update) → Update → **0.1.0 → 0.1.x installed**, from
Home and from Settings; "Later" (focus back on the hero); and an up-to-date app
showing no banner.

### Not verified / not built

- A **real torrent swarm** and real streamer — the fake honours the contract, but
  timing, peers and `PrioritizeSeek` under load are untested from the TV.
- **Real hardware**: HEVC / 10-bit / Dolby audio decode, 4K output, remote quirks.
- The updater on **Android 7** (no per-app install permission) and on a TV with
  **no "install unknown apps" screen** (the panel's fallback to `/tv`); only the
  Android 16 emulator was used.
  Only the plain Android TV image, not Google TV's.
- No instrumented (on-device) UI tests; the emulator runs were driven by hand.
- Bookmarks ("xem sau") UI and server-side resume sync.
- Subtitle **charset detection** — a CP1258 `.srt` is mojibake here as on the web.
- **Signed stream URLs** — the stream endpoint is unauthenticated for every
  client (see `viewer/CLAUDE.md`); tracked separately.

# CLAUDE.md — manager/

Guidance for the **streamer manager** control plane
(`module github.com/chiengyn/phimtor2/manager`). File paths below are relative to
`manager/`. For the repo-wide picture and the other services, see the root
[`../CLAUDE.md`](../CLAUDE.md).

## What this is

A **control plane** that load-balances torrents across multiple
[`streamer/`](../streamer/CLAUDE.md) instances. It is **not browser-facing**: only
the admin and viewer servers call it (server-to-server), and streamers register
themselves with it. The browser never talks to the manager — it streams directly
from the owning streamer's public URL, which the manager hands back on add/list.

Every route is **bearer-token gated**, so it is published at `$MANAGER_HOST`
(while keeping its `phimtor2-manager` kamal-network alias) so streamers on **other
servers** can register and be reached cross-host. Same-host admin/viewer + local
streamers use the faster internal `http://phimtor2-manager:8083`; only remote
streamers need the public host.

Why it exists: a torrent is **sticky** to the streamer that added it (pieces live
on that instance's disk; other instances 404). The manager assigns each new
torrent to a streamer, records `infohash→instance`, and routes later
get/delete by owner. Streamers **self-register** and heartbeat; an instance that
stops heartbeating for `MANAGER_HEARTBEAT_TTL` is dropped.

## Commands

```bash
go build -o phimtor2-manager .   # build (static CGO_ENABLED=0 binary)
go run .                         # run (listens on :8083)
go vet ./...
```

Targets Go 1.26. No UI, no DB, no disk state — the smallest of the four images.

## Configuration (`config.go`)

Env vars with matching CLI flags; tokens are env-only. See `.env.example`.

- HTTP: `MANAGER_PORT` (8083).
- Auth (three bearer tokens, all empty ⇒ gate disabled for dev):
  - `MANAGER_REGISTER_TOKEN` — streamers send it to register/heartbeat.
  - `MANAGER_INTERNAL_TOKEN` — admin/viewer send it on the control routes.
  - `STREAMER_INTERNAL_TOKEN` — the manager sends it to streamers' internal
    routes; defaults to `MANAGER_INTERNAL_TOKEN` (one secret for the whole plane).
- Registry: `MANAGER_HEARTBEAT_TTL` (30s), `MANAGER_RECONCILE_INTERVAL` (60s).
- Placement: `MANAGER_LB_STRATEGY` (`least-torrents` default, or `round-robin`).
- `MANAGER_FORWARD_TIMEOUT` (10s) — per control-call timeout to streamers.

## Architecture

Flat single `main` package, mirroring `streamer/`.

- **`server.go`** — chi router. Two token-gated route groups plus a `/up` that
  never fans out (so the manager reports up even when a streamer is down):
  - **Registration** (`MANAGER_REGISTER_TOKEN`): `POST /api/instances/{register,
    heartbeat,deregister}`.
  - **Control** (`MANAGER_INTERNAL_TOKEN`): `POST /api/torrents` (place + add →
    `{infoHash, streamerPublicURL}`), `GET /api/torrents` (aggregated, each entry
    annotated with its owner's `streamerPublicURL`), `GET /api/torrents/{hash}`,
    `DELETE /api/torrents/{hash}`, and `GET /admin/instances` (dashboard status).
- **`registry.go`** — the live instance set (`instances` by ID) and the
  `owners` map (`infohash→instance`). Background loops (`Run`): an expiry
  **sweep** drops instances past the heartbeat TTL, and a **reconcile** fans out
  `GET /api/torrents` to rebuild the owner map from ground truth (covers the
  streamer idle reaper, out-of-band adds, and torrents that moved instances).
- **`router.go`** — the control orchestration on `Registry`: `placeAdd` (pick →
  forward → record owner; magnet `btih` dedupe so a re-add never double-places),
  `resolveOwner`/`reResolve` (the 3-tier self-heal: trust the map, verify on a
  probe, re-probe all instances on a miss), `getTorrent`/`deleteTorrent`
  (delete is idempotent), `aggregateList`.
- **`instance.go`** — `Instance{ID, InternalURL, PublicURL, lastSeen}` plus the
  authenticated `do` helper for internal calls.
- **`loadbalance.go`** — `Placer`: `least-torrents` (fewest owned, from the owner
  map — zero extra calls) or `round-robin`.

The owner map is a **cache, never truth** — every owner-scoped op verifies via a
probe and self-heals on 404, so a stale entry (e.g. after the streamer reaper
evicts a torrent) is never fatal. A manager restart self-heals too: instances
re-register on their next heartbeat and the owner map rebuilds on the next
reconcile (and once at startup).

## Docker

`Dockerfile` builds a static `CGO_ENABLED=0` binary on a distroless base — no
ffmpeg, no assets, no volume. `EXPOSE 8083`. Deployed at `$MANAGER_HOST` (token
auth, plus the internal kamal alias) via `config/deploy.manager.yml`; see
[`../DEPLOY.md`](../DEPLOY.md).

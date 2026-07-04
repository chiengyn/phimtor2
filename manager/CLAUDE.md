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

Targets Go 1.26. No UI, no DB. It **does** persist one small file — the streamer
enrollment allow-list (`<MANAGER_STATE_DIR>/enrollments.json`) — so operator
approvals survive restarts; otherwise the smallest of the four images.

## Streamer enrollment (approve-pending + TOFU)

Streamers are **not** trusted by a shared secret. Each streamer self-generates and
persists its own identity token (`controlToken`); on first contact the manager parks
it as **pending** and an operator **approves** it in the admin Streamers dashboard
after eyeballing its advertised id/URLs. The manager **pins** that token's
fingerprint (sha256) on approval, so no other machine can later hijack an approved
id. The `controlToken` is the credential the manager presents on every
manager→streamer control call. On each approved register the manager also mints a
per-instance `sessionToken` that the streamer presents on heartbeat/deregister.
`MANAGER_REGISTER_TOKEN` survives only as a fleet-wide **anti-spam gate** on
`/register`. See `enrollment.go`.

## Configuration (`config.go`)

Env vars with matching CLI flags; tokens are env-only. See `.env.example`.

- HTTP: `MANAGER_PORT` (8083).
- Auth (both bearer tokens empty ⇒ gate disabled for dev):
  - `MANAGER_REGISTER_TOKEN` — shared join token streamers send at `/register`
    (anti-spam gate only; not a per-streamer identity).
  - `MANAGER_INTERNAL_TOKEN` — admin/viewer send it on the control routes (also
    gates the `/admin/*` enrollment routes).
- State: `MANAGER_STATE_DIR` (`./data`) — directory for `enrollments.json`.
- Registry: `MANAGER_HEARTBEAT_TTL` (30s), `MANAGER_RECONCILE_INTERVAL` (60s).
- Placement: `MANAGER_LB_STRATEGY` (`least-torrents` default, `least-bandwidth`,
  or `round-robin`). `least-bandwidth` balances by each streamer's live viewer
  egress rate — HTTP bytes/sec served to browsers (polled from the streamer's
  `/api/load` each reconcile) — with torrent count as the tiebreak when the fleet
  is idle.
- `MANAGER_FORWARD_TIMEOUT` (10s) — per control-call timeout to streamers.

## Architecture

Flat single `main` package, mirroring `streamer/`.

- **`server.go`** — chi router plus a `/up` that never fans out (so the manager
  reports up even when a streamer is down):
  - **Registration**: `POST /api/instances/register` is gated by the shared
    `MANAGER_REGISTER_TOKEN` and then runs the enrollment `verify` (approved →
    `{sessionToken}`; pending → `403 {status:pending}`; fingerprint mismatch →
    `401`). The payload also carries the streamer's self-reported `version` +
    `settings`, stored opaquely on the `Instance` (`InstanceMeta`, refreshed on
    every register) and surfaced by `/admin/instances`; the version is also
    recorded on the enrollment (`LastVersion`) so a still-pending streamer's
    build is visible before approval. `POST /api/instances/{heartbeat,deregister}` are gated instead by the
    per-instance `sessionToken` (validated in-handler — they can't share the join
    token's group).
  - **Control** (`MANAGER_INTERNAL_TOKEN`): `POST /api/torrents` (place + add →
    `{infoHash, streamerPublicURL}`), `GET /api/torrents` (aggregated, each entry
    annotated with its owner's `streamerPublicURL`), `GET /api/torrents/{hash}`,
    `DELETE /api/torrents/{hash}`, `GET /admin/instances` (dashboard status), and
    the enrollment routes `GET /admin/enrollments`,
    `POST /admin/enrollments/{id}/approve`, `DELETE /admin/enrollments/{id}`.
- **`registry.go`** — the live instance set (`instances` by ID) and the
  `owners` map (`infohash→instance`). Background loops (`Run`): an expiry
  **sweep** drops instances past the heartbeat TTL, and a **reconcile** fans out
  `GET /api/torrents` to rebuild the owner map from ground truth (authoritative:
  it prunes entries an instance no longer reports, so a reaper eviction can't
  leave a ghost that skews placement; covers out-of-band adds and torrents that
  moved instances too) and polls `GET /api/load` to refresh each instance's live
  viewer egress rate for `least-bandwidth` placement.
- **`router.go`** — the control orchestration on `Registry`: `placeAdd` (pick →
  forward → record owner; magnet `btih` dedupe so a re-add never double-places),
  `resolveOwner`/`reResolve` (the 3-tier self-heal: trust the map, verify on a
  probe, re-probe all instances on a miss), `getTorrent`/`deleteTorrent`
  (delete is idempotent), `aggregateList`.
- **`instance.go`** — `Instance{ID, InternalURL, PublicURL, ControlToken,
  SessionToken, lastSeen}` plus the `do` helper that authenticates internal calls
  with the instance's own `ControlToken`, and `newRandomToken` for session tokens.
- **`enrollment.go`** — the persisted streamer allow-list (`EnrollmentStore`): JSON
  file with atomic writes, `verify`/`approve`/`revoke`/`list`, sha256 fingerprint
  pinning. The only manager state on disk.
- **`loadbalance.go`** — `Placer`: `least-torrents` (fewest owned, from the owner
  map — zero extra calls), `least-bandwidth` (lowest live viewer egress rate,
  polled per-instance into `Instance.egressSpeed` by the reconcile loop's
  `refreshLoad`; ties fall back to torrent count then ID), or `round-robin`.

The owner map is a **cache, never truth** — every owner-scoped op verifies via a
probe and self-heals on 404, so a stale entry (e.g. after the streamer reaper
evicts a torrent) is never fatal. A manager restart self-heals too: instances
re-register on their next heartbeat and the owner map rebuilds on the next
reconcile (and once at startup).

## Docker

`Dockerfile` builds a static `CGO_ENABLED=0` binary on a distroless base — no
ffmpeg, no assets. It now needs a small **volume** for `MANAGER_STATE_DIR`
(`enrollments.json`). `EXPOSE 8083`. Deployed at `$MANAGER_HOST` (token auth, plus
the internal kamal alias) via `config/deploy.manager.yml`; see
[`../DEPLOY.md`](../DEPLOY.md).

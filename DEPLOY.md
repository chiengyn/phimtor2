# Deploying phimtor2 with Kamal

The services deploy to a **single host** with [Kamal](https://kamal-deploy.org),
each as its own app behind the shared `kamal-proxy` (host-based routing + automatic
Let's Encrypt TLS). MariaDB (a lighter, wire-compatible MySQL drop-in) runs as a
Kamal **accessory** on the same host, tuned for a small (1 GB) box.

| Service     | Config file                      | Image (ghcr.io)              | Domain (example)        | Port |
|-------------|----------------------------------|------------------------------|-------------------------|------|
| admin       | `config/deploy.yml`              | `chiengyn/phimtor2-admin`    | `$ADMIN_HOST`    | 8081 |
| viewer      | `config/deploy.viewer.yml`       | `chiengyn/phimtor2-viewer`   | `$VIEWER_HOST`   | 8082 |
| manager     | `config/deploy.manager.yml`      | `chiengyn/phimtor2-manager`  | `$MANAGER_HOST`  | 8083 |
| streamer 1  | `config/deploy.streamer.yml`     | `chiengyn/phimtor2-streamer` | `$STREAMER1_HOST` | 8080 |
| streamer 2  | `… -d s2` (`deploy.streamer.s2.yml`) | `chiengyn/phimtor2-streamer` | `$STREAMER2_HOST` | 8080 |

`admin` owns the schema and runs migrations on startup; `viewer` reads the same
database read-only. Admin and viewer share an on-host volume (`phimtor2_subtitles`)
for the local subtitle blob store. The **manager** is published at `$MANAGER_HOST`
(every route is bearer-token gated, so public exposure is safe) **and** keeps its
kamal-network alias: same-host admin/viewer + local streamers use the faster
internal `http://phimtor2-manager:8083`, while streamers on **other servers**
register over the public `https://$MANAGER_HOST`. The **streamers self-register**
with it. `$MANAGER_HOST` needs a DNS record before public HTTPS works, but the
deploy itself health-checks the container directly and succeeds without it.

**Multiple streamers** use Kamal **destinations** (one image, one `service:
phimtor2-streamer`, so the image's service label matches): instance 1 is the base
`config/deploy.streamer.yml`; each extra instance is a destination override (e.g.
`config/deploy.streamer.s2.yml`) deployed with `-d s2`, giving it its own public
domain, network-alias, and `/data` volume.

**Images are built by CI, not by Kamal.** `.github/workflows/docker.yml` builds
and pushes all four images to GHCR on every push to `main` (and `v*` tags),
tagging each with the full git SHA — the exact tag Kamal resolves from `git
rev-parse HEAD`. So deploys **pull** the prebuilt image with `--skip-push`; Kamal
never builds. The catch: **deploy from the same commit CI built.** Check out (or
`git pull`) the commit whose images are in GHCR before deploying, or pin it
explicitly with `kamal deploy --version=<sha> --skip-push`.

## 1. Prerequisites

- A Linux host with Docker installed, reachable via SSH as a sudo/root user.
- DNS A/AAAA records for the three domains pointing at the host.
- A GitHub PAT with `read:packages` (so the host can *pull* the CI-built images
  from ghcr.io; not needed if you make the packages public). CI itself pushes
  with the built-in `GITHUB_TOKEN`, so no push PAT is required.
- Kamal locally: `gem install kamal` (Ruby 3.2+).
- The images already built in GHCR — push to `main` first and let the workflow
  finish (check the Actions tab) before the first deploy.

## 2. Host config & secrets (this repo is public — nothing infra is committed)

The configs read the host and domains from the **environment** via ERB
(`<%= ENV["SERVER_IP"] %>`, etc.), so you never edit/commit your IP or domains.
Secrets resolve through the committed `.kamal/secrets` file the same way.

Put everything in a gitignored `.env` (copy `.kamal/secrets.example`) and source
it before running kamal:

```bash
set -a && source .env && set +a
```

| Variable | Kind | Example |
|----------|------|---------|
| `SERVER_IP` | host | `203.0.113.10` |
| `ADMIN_HOST` | host | `admin.yourdomain.com` |
| `VIEWER_HOST` | host | `yourdomain.com` |
| `MANAGER_HOST` | host | `manager.yourdomain.com` (token-gated; needs DNS) |
| `STREAMER1_HOST` | host | `stream1.yourdomain.com` |
| `STREAMER2_HOST` | host | `stream2.yourdomain.com` |
| `KAMAL_REGISTRY_PASSWORD` | secret | GitHub PAT with `read:packages` |
| `ADMIN_PASSWORD` | secret | admin Basic-auth password |
| `TMDB_API_KEY` | secret | themoviedb.org v3 key |
| `OPENSUBTITLES_API_KEY` | secret | opensubtitles.com v1 key |
| `SUBSOURCE_API_KEY` | secret | subsource.net key (alt subtitle source) |
| `DB_PASSWORD` | secret | app DB user password |
| `MYSQL_ROOT_PASSWORD` | secret | MySQL root password |
| `MANAGER_REGISTER_TOKEN` | secret | shared join token for streamer `/register` (anti-spam gate; `openssl rand -hex 32`) |
| `MANAGER_INTERNAL_TOKEN` | secret | admin/viewer → manager |

The manager→streamer credential is no longer a configured secret: each streamer
self-generates a `controlToken` (persisted at `/data/identity`) that the manager
pins when an operator **approves** the streamer in the admin Streamers dashboard.
A new streamer stays **pending** (and its `/api/torrents` adds fail) until approved.
The manager persists the approval allow-list to its `/data` volume
(`MANAGER_STATE_DIR`), so approvals survive redeploys; keep each streamer's
`/data` volume and `STREAMER_INSTANCE_ID` stable so its approval survives too.

The only thing still hardcoded in the configs is the GHCR image namespace
(`image: chiengyn/phimtor2-*`) — change `chiengyn` if you fork under another user.

## 4. First deploy

`kamal setup --skip-push` bootstraps the host (installs kamal-proxy, logs into
the registry), boots the MariaDB accessory, then **pulls** the CI-built image and
deploys it. Run it for admin first so MariaDB comes up and migrations run:

```bash
kamal setup --skip-push                                     # admin: host + proxy +
                                                            # registry login + MariaDB
kamal deploy -c config/deploy.manager.yml    --skip-push    # manager (before streamers)
kamal deploy -c config/deploy.streamer.yml   --skip-push    # streamer 1
# kamal deploy -c config/deploy.streamer.yml -d s2 --skip-push   # streamer 2 (optional)
kamal deploy -c config/deploy.viewer.yml     --skip-push    # viewer (reads admin's DB)
```

`kamal setup` (admin) installs the shared kamal-proxy and logs the host into the
registry, so the other services only need `kamal deploy ... --skip-push`. Deploy
the **manager before the streamers** so they can register on boot.

## 5. Subsequent deploys

Pull the commit whose images CI has published, then deploy with `--skip-push`:

```bash
git pull                                                  # match CI's built commit
set -a && source .env && set +a
kamal deploy                                  --skip-push  # admin
kamal deploy -c config/deploy.manager.yml     --skip-push  # manager
kamal deploy -c config/deploy.streamer.yml    --skip-push  # streamer 1
# kamal deploy -c config/deploy.streamer.yml -d s2 --skip-push  # streamer 2 (optional)
kamal deploy -c config/deploy.viewer.yml      --skip-push  # viewer
```

> `--skip-push` makes Kamal pull `…/phimtor2-<svc>:<git-sha>` instead of
> building. If you deploy from a commit CI hasn't built yet, the pull 404s — wait
> for the workflow, or build locally by dropping `--skip-push`.

## 6. Deploy from GitHub Actions (manual)

`.github/workflows/deploy.yml` runs the same `kamal deploy --skip-push` from CI,
but **only when you trigger it** — it has no `push` trigger. Use it instead of
deploying from your laptop once the server is bootstrapped.

How to run it: **Actions tab → "Deploy (manual)" → Run workflow**, then pick the
branch/tag to deploy and which service (`all`/`admin`/`viewer`/`manager`/`streamers`).

One-time setup before the first manual run (Settings → Secrets and variables →
Actions):

1. Bootstrap the host once from a workstation (CD only does `deploy`, not
   `setup`): `kamal setup --skip-push`.
2. Add the repo **Secrets**: `KAMAL_SSH_PRIVATE_KEY` (key authorized on the
   server for the Kamal SSH user, root by default), `KAMAL_REGISTRY_PASSWORD`,
   `ADMIN_PASSWORD`, `TMDB_API_KEY`, `OPENSUBTITLES_API_KEY`, `SUBSOURCE_API_KEY`,
   `DB_PASSWORD`, `MYSQL_ROOT_PASSWORD`, `MANAGER_REGISTER_TOKEN`,
   `MANAGER_INTERNAL_TOKEN`.
3. Add the repo **Variables** (non-secret, read by the configs via ERB):
   `SERVER_IP`, `ADMIN_HOST`, `VIEWER_HOST`, `MANAGER_HOST`, `STREAMER1_HOST`, `STREAMER2_HOST`.
   (`SERVER_IP` also seeds `known_hosts`, replacing a separate `SSH_HOST`.)
4. *(Optional approval gate)* Create a `production` **environment** with required
   reviewers — each run then pauses for sign-off before deploying.

Note: `.kamal/secrets` is committed (it holds only `${VAR}` references); the CD
job fills those from the GitHub secrets above, exactly like `.env` does locally.

## Notes & gotchas

- **Database hostname.** Apps reach MariaDB at `phimtor2-admin-mysql` over Kamal's
  shared `kamal` Docker network (this is `<service>-<accessory>`, and Kamal attaches
  accessories to that network automatically — do **not** re-add it via
  `options.network` or `docker run` errors with a duplicate `--network`). If you
  rename the admin `service:`, update `DB_HOST` in both the admin and viewer
  configs. Verify the actual name with `kamal accessory details mysql`.
- **Streamer reachability.** Each `$STREAMERn_HOST` must be reachable from end
  users' **browsers** — the watch pages call that streamer's stats/stream API
  directly (the streamer sends permissive CORS headers on those public routes).
  The **manager** reaches each streamer server-to-server at
  `http://phimtor2-streamer-N:8080` over the kamal network for the token-gated
  control routes (add/list/get/delete). For a SAME-host streamer those resolve via
  its **`network-alias: phimtor2-streamer-N`** (advertised to the manager as
  `STREAMER_ADVERTISE_INTERNAL_URL`). A streamer on **another server** instead
  advertises its **public** host as the internal URL (e.g.
  `STREAMER_ADVERTISE_INTERNAL_URL=https://streamerX.yourdomain.com`) — the
  manager reaches its token-gated control routes over the internet — and sets
  `MANAGER_URL=https://$MANAGER_HOST` to register. The **manager** is published at
  `$MANAGER_HOST` (token-gated) yet keeps its `phimtor2-manager` alias, so
  same-host admin/viewer still use `http://phimtor2-manager:8083`.
- **Streamer redeploys need the old container stopped first.** Each streamer holds
  an **exclusive lock** on its prefix-completion store, so it can't run two
  instances of the *same* config at once. Kamal's rolling deploy (start new →
  health-check → stop old) therefore fails with `open prefix completion: timeout`.
  Before redeploying a streamer, stop the running one
  (`docker stop <phimtor2-streamer-1-web-...>`) — a few seconds of downtime for
  *that* instance; the other instances keep serving. (admin/viewer/manager redeploy
  normally.)
- **Health checks.** Each service exposes an unauthenticated `GET /up` (admin's is
  exempt from Basic auth) that kamal-proxy uses before cutting traffic over.
- **Persistence.** MariaDB data, the subtitle store, and the streamer's torrent
  cache live in host volumes/directories and survive redeploys. The streamer's
  cache tier is intentionally wiped on container startup (by design).
- **Logs / status:** `kamal app logs -f`, `kamal app details`, `kamal proxy logs`
  (add `-c config/deploy.<svc>.yml` for the other services).

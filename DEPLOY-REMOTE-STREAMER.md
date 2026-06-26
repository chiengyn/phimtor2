# Deploying a streamer on a host you don't own

This guide adds an **extra streamer** to an existing phimtor2 deployment, running
on a **third-party box** (a rented VPS, a friend's server, any Linux host with
Docker) that is **not** part of your Kamal stack. The streamer self-registers
with your central **manager**, you **approve** it once in the admin dashboard, and
from then on it serves torrents like any other instance.

You do **not** need Kamal, MySQL, or your repo on the remote host — only Docker
and a public domain. Everything else is the prebuilt GHCR image.

## How a remote streamer fits the architecture

```
  browser ──HTTPS──> admin/viewer watch page
     │
     └──HTTPS (stats + stream, direct)──> THIS streamer  (public domain)
                                              ▲
  your manager ──HTTPS (add/list/delete, token-gated)──┘
     ▲
  THIS streamer ──HTTPS (register + heartbeat)──> your manager (MANAGER_HOST)
```

Two facts drive every requirement below:

1. **The browser talks to the streamer directly** for stats/stream. The watch
   pages are served over HTTPS, so the streamer **must** also be reachable over
   **HTTPS** (a plain-HTTP streamer is blocked as mixed content). That means a
   **public domain + TLS** in front of the streamer — handled here by **Caddy**,
   which gets a Let's Encrypt cert automatically.
2. **The manager reaches the streamer over the public internet** (it's on a
   different host, so there's no private Kamal network). So this streamer
   advertises its **public HTTPS URL** as *both* its public and its "internal"
   URL.

The streamer container itself only ever speaks plain HTTP on `:8080`; Caddy
terminates TLS and proxies to it.

## 1. Prerequisites

On the remote host:

- **Linux, x86-64 (amd64).** The published image is amd64-only.
- **Docker + Docker Compose v2** (`docker compose version`).
- A **public IPv4** with inbound **80 and 443** open (80 is needed for the
  Let's Encrypt HTTP challenge; 443 for actual traffic).
- Reasonable **disk** for the torrent cache (a few GB+) and **good bandwidth** —
  this box pulls from the torrent swarm and serves video to viewers.
- A **domain/subdomain** you can point at this host, e.g.
  `stream2.yourdomain.com`. Create a DNS **A record** → the host's IP before you
  start (Let's Encrypt won't issue a cert until DNS resolves).

From the phimtor2 operator (you / whoever runs admin + manager), collect:

| Value | What it is | Example |
|-------|-----------|---------|
| `MANAGER_URL` | Public URL of your manager | `https://manager.yourdomain.com` |
| `MANAGER_REGISTER_TOKEN` | Shared join token (anti-spam gate on `/register`) | the value from your stack's `.env` |
| A unique `STREAMER_INSTANCE_ID` | Stable id for this box in the registry | `streamer-remote-1` |
| The public domain for **this** streamer | Set up above | `stream2.yourdomain.com` |

> The manager→streamer credential is **not** something you set. On first boot the
> streamer generates its own identity token and persists it to `/data/identity`;
> the manager pins it when you approve the streamer. So keep the `/data` volume
> and `STREAMER_INSTANCE_ID` stable across restarts, or a redeploy looks like a
> brand-new pending streamer.

## 2. Image access

The image is `ghcr.io/chiengyn/phimtor2-streamer` (change `chiengyn` if you forked
under another owner). Tags: `latest` (tip of `main`), the full git SHA, or a
`vX.Y.Z` release tag — **pin a SHA or version tag in production** so a redeploy is
reproducible.

- If the GHCR package is **public**, no login is needed.
- If it's **private**, log the remote host in once with a GitHub PAT that has
  `read:packages`:

  ```bash
  echo "$GHCR_PAT" | docker login ghcr.io -u <github-username> --password-stdin
  ```

## 3. Project files on the remote host

Create a directory and three files.

```bash
mkdir -p ~/phimtor2-streamer && cd ~/phimtor2-streamer
```

### `.env`

```dotenv
# --- identity / where it registers ---
MANAGER_URL=https://manager.yourdomain.com
MANAGER_REGISTER_TOKEN=<the shared join token from the operator>
STREAMER_INSTANCE_ID=streamer-remote-1

# --- this streamer's public address (must match the Caddy domain below) ---
STREAMER_PUBLIC_HOST=stream2.yourdomain.com

# --- email Caddy uses for the Let's Encrypt account ---
ACME_EMAIL=you@example.com

# --- image tag: pin a SHA or vX.Y.Z in production instead of latest ---
STREAMER_IMAGE=ghcr.io/chiengyn/phimtor2-streamer:latest
```

### `docker-compose.yml`

```yaml
services:
  streamer:
    image: ${STREAMER_IMAGE}
    restart: unless-stopped
    environment:
      PORT: "8080"
      DATA_DIR: /data
      STORAGE_MODE: prefix-cache
      # Tune to the box. CACHE_MB is the on-disk budget for the bulk; raising it
      # lets more concurrent viewers be served without re-hitting the swarm.
      READAHEAD_MB: "16"
      PREFIX_MB: "32"
      CACHE_MB: "2048"
      MAX_CONNS: "100"
      IDLE_TTL_MIN: "20"
      RETAIN_HOT: "false"
      # --- manager registration ---
      MANAGER_URL: ${MANAGER_URL}
      MANAGER_REGISTER_TOKEN: ${MANAGER_REGISTER_TOKEN}
      STREAMER_INSTANCE_ID: ${STREAMER_INSTANCE_ID}
      # On a foreign host the manager reaches this streamer over the internet, so
      # the "internal" URL is ALSO the public HTTPS URL.
      STREAMER_ADVERTISE_PUBLIC_URL: https://${STREAMER_PUBLIC_HOST}
      STREAMER_ADVERTISE_INTERNAL_URL: https://${STREAMER_PUBLIC_HOST}
    volumes:
      # Persists the torrent prefix tier AND /data/identity (the approval token).
      # Keep this volume to keep the streamer's approval across redeploys.
      - streamer_data:/data
    expose:
      - "8080"          # only Caddy reaches it; not published to the host

  caddy:
    image: caddy:2
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy_data:/data
      - caddy_config:/config
    depends_on:
      - streamer

volumes:
  streamer_data:
  caddy_data:
  caddy_config:
```

### `Caddyfile`

```caddyfile
{$STREAMER_PUBLIC_HOST} {
	tls {$ACME_EMAIL}

	reverse_proxy streamer:8080 {
		# Video is large, long-lived, ranged. Flush immediately and never time
		# out a long open-ended range while the file streams.
		flush_interval -1
	}
}
```

> Caddy reads `{$STREAMER_PUBLIC_HOST}` / `{$ACME_EMAIL}` from the environment.
> Pass them in by adding `environment:` to the `caddy` service, **or** simpler:
> run compose with the `.env` already loaded (compose substitutes `${...}` in
> `docker-compose.yml`, and Caddy's `{$...}` needs them in its own env). To keep
> it foolproof, add this to the `caddy` service:
>
> ```yaml
>     environment:
>       STREAMER_PUBLIC_HOST: ${STREAMER_PUBLIC_HOST}
>       ACME_EMAIL: ${ACME_EMAIL}
> ```

## 4. Bring it up

```bash
docker compose pull
docker compose up -d
docker compose logs -f streamer
```

In the logs you should see it register and then get parked as **pending** — it
will keep retrying (the manager returns `403 pending` until you approve it). Caddy
will fetch a cert on first request to your domain.

Quick health check (should return `ok` / 200 once TLS is up):

```bash
curl -fsS https://stream2.yourdomain.com/up
```

## 5. Approve the streamer (operator step, once)

The new streamer serves **nothing** until an operator approves it. In your
**admin** UI:

1. Open the **Streamers** dashboard (`/streamers`).
2. Find the pending entry — verify its **instance id** and advertised
   **URLs** match what you set above (this is the trust-on-first-use check; the
   manager pins this streamer's identity fingerprint on approval).
3. Click **Approve**.

Or approve from the command line against the manager (the token is your
`MANAGER_INTERNAL_TOKEN`):

```bash
curl -X POST \
  -H "Authorization: Bearer $MANAGER_INTERNAL_TOKEN" \
  https://manager.yourdomain.com/admin/enrollments/streamer-remote-1/approve
```

Within a heartbeat the streamer flips to active and the manager starts placing
torrents on it. Confirm it shows up healthy in the Streamers dashboard.

## 6. Verify end to end

- The streamer appears **active** (not pending) in the admin Streamers dashboard.
- Add/play a title from the watch UI; if the manager places it on this instance,
  the browser's network tab shows `https://stream2.yourdomain.com/.../stream`
  returning `206 Partial Content` and the video plays.

## 7. Updating / redeploying

```bash
cd ~/phimtor2-streamer
# bump STREAMER_IMAGE in .env to the new SHA/tag, then:
docker compose pull
docker compose up -d
```

> **Stop the old container first if a redeploy hangs.** Each streamer holds an
> **exclusive lock** on its prefix-completion store, so two containers sharing the
> same `/data` can't run at once. Compose's recreate normally stops the old one
> before starting the new, so this is usually fine — but if you see
> `open prefix completion: timeout`, run `docker compose down` then
> `docker compose up -d`. As long as the `streamer_data` volume and
> `STREAMER_INSTANCE_ID` are unchanged, the approval survives and no re-approval
> is needed.

## 8. Troubleshooting

| Symptom | Likely cause / fix |
|---------|--------------------|
| Streamer logs show repeated `403` / "pending" | Not approved yet — do step 5. |
| Register fails with `401` | Identity fingerprint mismatch: this id was approved for a **different** token. You changed `STREAMER_INSTANCE_ID` reuse or lost `/data`. Revoke the old enrollment in the dashboard and re-approve, or use a fresh id. |
| Register fails with `unauthorized` before pending | `MANAGER_REGISTER_TOKEN` doesn't match the manager's. |
| Browser console: **mixed content** / blocked stream | The streamer URL isn't HTTPS, or DNS/cert isn't ready. Check `curl https://<host>/up` and Caddy logs (`docker compose logs caddy`). |
| Cert never issues | DNS A record not pointing here yet, or port 80 blocked (Let's Encrypt HTTP challenge). |
| Manager can't reach streamer (control calls fail) | The advertised internal URL must be the **public HTTPS** URL on a foreign host, and 443 must be open. |
| Plays for a few minutes then stops | A proxy in front is buffering/timing out — ensure `flush_interval -1` and that you didn't add an extra CDN/proxy with a short response timeout. |

## Notes

- **Adding more remote streamers:** repeat this whole guide on another host with a
  **different** `STREAMER_INSTANCE_ID` and a **different** public domain. They're
  interchangeable; the manager load-balances across all approved instances.
- **Removing one:** stop the container, then revoke its enrollment in the admin
  Streamers dashboard (or `DELETE .../admin/enrollments/<id>`) so the manager
  forgets it. An instance that stops heartbeating is dropped from the live
  registry automatically after the heartbeat TTL.
- This guide is the **off-Kamal** path. Streamers on a host you **do** control
  with Kamal use the destination flow in [`DEPLOY.md`](DEPLOY.md) instead.
```

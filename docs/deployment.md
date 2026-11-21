# Deployment Guide

Ourchive is designed to run on a single inexpensive VPS because logged-in file transfers, uploads, and `.zip` exports flow peer-to-peer over WebRTC rather than through the Hub.

---

## Hardware & VPS Sizing

| Users | Recommended VPS | Disk | Notes |
| :--- | :--- | :--- | :--- |
| 1 – 25 | 1 vCPU / 1 GB RAM | 20 GB SSD | Comfortable for small friend groups |
| 25 – 100 | 2 vCPU / 2 GB RAM | 40 GB SSD | Sweet spot for active communities |
| 100 – 250+ | 2 – 4 vCPU / 4 GB RAM | 80 GB SSD | Headroom for concurrent public embeds & thumbnails |

**Why a micro-VPS scales so well:**
- Logged-in downloads, uploads, and `.zip` downloads use **0 VPS egress** when WebRTC P2P connects (only ~2 KB of SDP signaling passes through the Hub).
- Thumbnails are tiny WebP images cached on the VPS (`data/thumbs/`) so browsing stays instant even when a contributor node is offline.
- Only **public share links** (`s.yourdomain.com`) or strict-NAT HTTP fallback relays consume VPS bandwidth — and Host Admins can cap or disable relay bandwidth, or offload public shares to S3 / Cloudflare R2 / Bunny.net.

---

## Prerequisites

Point **two DNS records** (`A` and/or `AAAA`) to your VPS IP address:

1. `archive.yourdomain.com` — Main Web UI, API, SSE, and Agent WebSocket tunnel.
2. `s.yourdomain.com` — Isolated share and embed subdomain (zero cookie overlap with the main app).

---

## Option 1: Docker Compose + Caddy (Recommended)

Caddy automatically provisions and renews Let's Encrypt HTTPS certificates for both domains.

1. Clone the repository and create `.env`:
   ```bash
   git clone <your-repo-url> ourchive
   cd ourchive
   cp .env.example .env
   ```

2. Edit `.env` with your domains:
   ```dotenv
   OURCHIVE_APP_DOMAIN=archive.yourdomain.com
   OURCHIVE_SHARE_DOMAIN=s.yourdomain.com
   ```

3. Build and start:
   ```bash
   docker compose up -d --build
   ```

4. Visit `https://archive.yourdomain.com` to complete **First-Run Setup** and create the initial `host_admin` account.

### Updating After a Git Pull (Zero Data Loss)
Your SQLite database (`./data/ourchive.db`) and thumbnail cache (`./data/thumbs/`) are mounted on the host outside the container, and schema migrations run automatically on startup:

```bash
git pull
docker compose up -d --build
```

### Backups
SQLite runs in WAL mode. For a clean point-in-time backup:

```bash
sqlite3 data/ourchive.db ".backup 'data/ourchive-backup-$(date +%F).db'"
```

*(If `sqlite3` is not installed on the host, stop the container briefly and copy `data/ourchive.db`.)*

---

## Option 2: Bare-Metal Linux (`systemd` + Caddy)

1. Build the Web UI and Go Hub binary:
   ```bash
   npm --prefix web ci
   npm --prefix web run build
   go build -o /opt/ourchive/ourchive-hub ./cmd/hub
   ```

2. Copy [deploy/ourchive-hub.service](../deploy/ourchive-hub.service) to `/etc/systemd/system/ourchive-hub.service`, adjust the `Environment=` domains, and enable it:
   ```bash
   sudo systemctl daemon-reload
   sudo systemctl enable --now ourchive-hub
   ```

3. Configure Caddy using [deploy/Caddyfile](../deploy/Caddyfile) (pointing `reverse_proxy` to `127.0.0.1:8080`). Note that `flush_interval -1` is required so real-time SSE streams and large file streams are not buffered.

---

## Environment Variables

| Variable | Default | Description |
| :--- | :--- | :--- |
| `OURCHIVE_ADDR` | `:8080` | Bind address for the Hub HTTP server |
| `OURCHIVE_DB_PATH` | `data/ourchive.db` | Path to the SQLite WAL database |
| `OURCHIVE_THUMB_DIR` | `data/thumbs` | Directory for Hub-cached WebP thumbnails |
| `OURCHIVE_WEB_DIR` | `web/dist` | Directory containing the built React SPA |
| `OURCHIVE_DIST_DIR` | `dist` | Directory containing cross-platform `ourchive-agent-*` binaries served at `/downloads/*` |
| `OURCHIVE_PUBLIC_URL` | `http://localhost:8080` | Public origin of the Hub (used for WebAuthn RP ID/Origin, OIDC callbacks, and default `/downloads`) |
| `OURCHIVE_HUB_NAME` | *(hostname of `OURCHIVE_PUBLIC_URL`)* | Display name shown in the top-right active Hub status monitor |
| `OURCHIVE_AGENT_DOWNLOAD_URL` | *(empty)* | Optional override for official signed Agent releases (e.g. GitHub Releases); defaults to `<OURCHIVE_PUBLIC_URL>/downloads` when unset |
| `OURCHIVE_AGENT_MANIFEST_URL` | *(empty)* | Optional override for the auto-update `manifest.json` URL |
| `OURCHIVE_SHARE_HOST` | `s.localhost` | Hostname routed to the isolated share handler |
| `OURCHIVE_SHARE_URL` | `http://s.localhost:8080` | Base URL used when generating share links |

---

## Relay Guardrails & Optional Edge Storage

In **Admin → Settings**, Host Admins can configure:

- **Hub Relay Fallback**: Enable/disable HTTP fallback when P2P is blocked, set a maximum relay file size, and set a per-stream bandwidth cap (bytes/sec) to protect VPS egress.
- **Edge Storage (S3 / Cloudflare R2 / Bunny.net)**: Optionally offload public share payloads and thumbnails to an S3-compatible bucket or CDN so viral Discord/web embeds never touch VPS bandwidth.
- **OIDC Identity Providers**: Add Google, Authentik, Keycloak, Pocket ID, or any standard OIDC Discovery provider directly from the UI (no restart required).

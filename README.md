# Ourchive

A semi-private, decentralized pooled cloud storage, rich file previewer, isolated embed link generator, and 3-tier GPU media converter platform.

## Architecture
- **Ourchive Hub** (`cmd/hub`): Single-binary Go control plane serving the built React Web UI, SQLite WAL database (auto-migrating), Argon2id + invite-only auth, tiered audit logs, `WSS + yamux` Agent tunnel broker, WebRTC P2P signaling, opt-in S3/Bunny.net edge storage, and isolated `s.` share subdomain embeds.
- **Headless Share & Compute Agent** (`cmd/agent`): Cross-platform Go daemon behind home NAT with `os.OpenRoot` jailed folder/file serving, multi-hub configs under `$HOME/.config/ourchive/hubs/`, WebRTC P2P DataChannels, hardware-verified GPU `ffmpeg` conversion, user-level logon service installer, and verified self-updater.
- **Web GUI** (`web/`): React + TypeScript + Vite interface.

---

## Local Development (Windows)

```powershell
.\dev.ps1 -Agent
```

- Hub API & static UI: `http://localhost:8080`
- Vite dev server (HMR): `http://localhost:5173`
- Isolated share subdomain: `http://s.localhost:8080/<slug>`

---

## VPS Production Deployment (Docker + Caddy)

1. Point two DNS A/AAAA records to your VPS:
   - `archive.yourdomain.com` (main app & API)
   - `s.yourdomain.com` (isolated share embeds — zero cookie overlap)
2. Clone the repo and configure `.env`:
   ```bash
   cp .env.example .env
   # Edit OURCHIVE_APP_DOMAIN and OURCHIVE_SHARE_DOMAIN
   ```
3. Build and start (Caddy automatically provisions Let's Encrypt HTTPS certificates):
   ```bash
   docker compose up -d --build
   ```

### Updating After a Git Push (Zero DB Loss)
Your SQLite database (`./data/ourchive.db`) and thumbnail cache (`./data/thumbs/`) live on the host outside the container, and migrations run automatically on startup:
```bash
git pull
docker compose up -d --build
```

### Database Backup
```bash
cp data/ourchive.db data/ourchive-backup-$(date +%F).db
```

For bare-metal Linux without Docker, see [deploy/ourchive-hub.service](deploy/ourchive-hub.service) and [deploy/Caddyfile](deploy/Caddyfile).

---

## Contributor Agent Setup

1. Generate an enrollment token in **Contribution settings → Connect a machine**.
2. Enroll your machine (configs are stored per-Hub in `$HOME/.config/ourchive/hubs/`):
   ```bash
   ourchive-agent enroll --hub https://archive.yourdomain.com --token <TOKEN>
   ```
   Optional flags: `--read-only` (blocks all remote writes/deletes at the Agent), `--allow-dir <path>` (repeatable local whitelist).
3. Install silent auto-start on login (no admin/UAC required):
   ```bash
   ourchive-agent service install
   ```
4. Opt into silent signed auto-updates (optional):
   ```bash
   ourchive-agent update --url https://api.github.com/repos/<owner>/<repo>/releases/latest --enable-auto
   ```
5. List enrolled hubs or run in the foreground:
   ```bash
   ourchive-agent list
   ourchive-agent run
   ```

---

## Building Cross-Platform Agent Binaries

- **Windows**: `.\scripts\build-agent.ps1 -Version v0.3.0`
- **Linux / macOS**: `./scripts/build-agent.sh v0.3.0`
- **Automated GitHub Releases**: Push a tag (`git tag v0.3.0 && git push --tags`) to trigger `.github/workflows/release-agent.yml`.

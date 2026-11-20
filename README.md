# Ourchive

Semi-private pooled storage, on-demand media previewing, low-egress social clip embeds, and 3-tier GPU/WebCodecs/WASM file conversion.

## Structure

- `cmd/hub` — Ourchive Hub (API, RBAC auth, tunnel broker, dedicated share subdomain router)
- `cmd/agent` — Headless Share & Compute Agent (local folders + native `ffmpeg` GPU probe/worker)
- `internal/db` — SQLite WAL / PostgreSQL-compatible schema & embedded migrations
- `pkg/protocol` — Shared Hub/Agent protocol types
- `web/` — React + TypeScript + Vite web interface (Warm Paper + Geist design tokens)
- `design/ui-prototype.html` — Early static UI design exploration

## Local Development

### 1. Start the Hub
```powershell
go run ./cmd/hub
```
Listens on `http://localhost:8080` (and routes `http://s.localhost:8080` to the isolated share server).

### 2. Probe Local GPU / Compute Capabilities
```powershell
go run ./cmd/agent probe
```

### 3. Start the Web UI
```powershell
cd web
npm install
npm run dev
```
Opens at `http://localhost:5173` with `/api` proxied to the Hub.

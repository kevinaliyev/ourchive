# Contributor Agent Guide

The **Ourchive Agent** (`ourchive-agent`) is a single-binary headless daemon for **Windows**, **macOS**, and **Linux**. It connects outbound over WebSocket (`WSS + yamux`) to one or more Ourchive Hubs behind home NAT—no port forwarding, UPnP, or admin privileges required.

---

## Quick Install (No Go Required)

First, generate a one-time enrollment token in the Web UI under **Contribution settings → Connect a machine**.

### Windows (PowerShell)
```powershell
$repo="OWNER/REPO"; $hub="https://archive.yourdomain.com"; $tok="TOKEN"
Stop-Process -Name ourchive-agent -Force -ErrorAction SilentlyContinue
$dir="$env:LOCALAPPDATA\Ourchive"; New-Item -ItemType Directory -Force $dir | Out-Null
$exe="$dir\ourchive-agent.exe"
Invoke-WebRequest "https://github.com/$repo/releases/latest/download/ourchive-agent-windows-amd64.exe" -OutFile $exe
& $exe enroll --hub $hub --token $tok
& $exe update --url "https://api.github.com/repos/$repo/releases/latest" --enable-auto
& $exe service install
Start-Process $exe -ArgumentList "run" -WindowStyle Hidden
```

### macOS & Linux (Bash)
```bash
REPO="OWNER/REPO" HUB="https://archive.yourdomain.com" TOK="TOKEN" bash -c '
  OS=$(uname -s | tr "[:upper:]" "[:lower:]")
  ARCH=$(uname -m | sed -e "s/x86_64/amd64/" -e "s/aarch64/arm64/")
  DIR="$HOME/.local/bin"; mkdir -p "$DIR"; BIN="$DIR/ourchive-agent"
  curl -fsSL "https://github.com/$REPO/releases/latest/download/ourchive-agent-$OS-$ARCH" -o "$BIN"
  chmod +x "$BIN"
  "$BIN" enroll --hub "$HUB" --token "$TOK"
  "$BIN" update --url "https://api.github.com/repos/$REPO/releases/latest" --enable-auto
  "$BIN" service install
'
```

*(Append `--read-only` to the `enroll` command to enforce local read-only mode on the machine.)*

---

## CLI Reference

| Command | Description |
| :--- | :--- |
| `ourchive-agent enroll --hub <url> --token <tok>` | Generates an Ed25519 keypair, enrolls with the Hub, and writes a per-Hub config file |
| `ourchive-agent run` | Runs the daemon in the foreground for all enrolled Hubs concurrently (`--max-retries N`, default `5`, `0` = unlimited) |
| `ourchive-agent logs [-n 80]` | Prints the OS user log file path and recent agent log entries |
| `ourchive-agent list` | Lists all enrolled Hubs and their config file paths |
| `ourchive-agent probe` | Prints detected CPU, discrete GPU, and `ffmpeg` hardware encoder capabilities |
| `ourchive-agent service install` | Installs a zero-UAC / user-level logon service (Registry Run key / launchd / systemd user unit) |
| `ourchive-agent service uninstall` | Removes the user-level logon service |
| `ourchive-agent update [--url <release-api>] [--enable-auto]` | Checks for a newer release, verifies SHA-256 + Ed25519 signature, and replaces the binary |

### Useful `enroll` Flags
- `--name <label>`: Custom display name for this machine (defaults to hostname).
- `--read-only`: Hard-blocks all remote uploads, folder creation, renames, and deletes at the Agent layer regardless of Hub settings.
- `--allow-dir <path>`: Repeatable whitelist restricting which directories on disk can ever be mounted through the UI.

---

## Multi-Hub Configuration

A single Agent process can simultaneously serve multiple independent Ourchive Hubs. Configs are stored per-Hub under:

```text
$HOME/.config/ourchive/
├── settings.json                 # Global auto-update preferences
└── hubs/
    ├── archive-yourdomain-com.json
    └── localhost-8080.json
```

Each Hub config contains its own Ed25519 private key, node ID, mount definitions, upload/download bandwidth caps, and read-only policy. Compromising or leaving one Hub never affects another.

---

## Agent Logs & Retry Behavior

All `ourchive-agent run` output (connections, config pushes, and errors) is written to both stderr and a persistent per-user log file (auto-rotated at 5 MiB):

- **Windows**: `%LOCALAPPDATA%\Ourchive\logs\agent.log`
- **macOS**: `~/Library/Logs/Ourchive/agent.log`
- **Linux**: `$XDG_STATE_HOME/ourchive/logs/agent.log` (default `~/.local/state/ourchive/logs/agent.log`)

Run `ourchive-agent logs` to print the log path and recent entries.

By default, each enrolled Hub stops retrying after **5 consecutive failed attempts** (`--max-retries 5`; pass `--max-retries 0` for unlimited retries). When running in an interactive terminal and all Hubs fail, the shell stays open so you can **retry (`r`)**, **print recent logs (`l`)**, or **exit (`q`)**.

---

## Filesystem Safety & Features

- **`os.OpenRoot` Jailing**: Every mounted folder is accessed strictly through Go's `os.OpenRoot`, preventing symlink escapes and `../` traversal.
- **Crash-Safe Staged Uploads**: Incoming uploads write to `<target>.ourchive.part`, support byte-offset resume, verify optional SHA-256 checksums, `Sync()` to disk, and atomically rename into place.
- **Fast-Hash Relocation**: If you move or rename a shared file locally on disk, Ourchive can re-locate it by fast content fingerprint without breaking existing share links.
- **Cross-Platform OS Integrations**: Native folder/file pickers and "Reveal in Explorer / Finder / File Manager" work on Windows, macOS, and Linux, restricted exclusively to the node's owner.
- **Signed Auto-Updates**: Releases include `manifest.json` with SHA-256 checksums and optional Ed25519 signatures verified before swapping the executable.

---

## Building Cross-Platform Binaries

- **Windows**: `.\scripts\build-agent.ps1 -Version v0.3.0`
- **macOS / Linux**: `./scripts/build-agent.sh v0.3.0`
- **GitHub Actions**: Pushing a tag (`git tag v0.3.0 && git push --tags`) triggers [.github/workflows/release-agent.yml](../.github/workflows/release-agent.yml) to build and publish binaries for Windows (`amd64`), macOS (`amd64`, `arm64`), and Linux (`amd64`, `arm64`).

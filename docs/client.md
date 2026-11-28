# Desktop Client

The **Ourchive app** (`client/`) is a native desktop application for members and contributors. It
shows the same React UI the hub serves, in a native window, and on desktop platforms it also runs
the contributor agent in the same process. It replaces the old routine of keeping an
`ourchive-agent run` console window open.

| | Hub website | Desktop app | Headless agent (`ourchive-agent`) |
| :--- | :--- | :--- | :--- |
| Browse, preview, upload, share | ✅ | ✅ | — |
| Several hubs side by side | one per site | ✅ hub switcher | ✅ |
| Contribute this computer | via the agent | ✅ built in | ✅ |
| Runs with no window | — | ✅ tray | ✅ service |
| Platforms | any browser | Windows, macOS, Linux (mobile: members only) | Windows, macOS, Linux, NAS |

Use the headless agent for servers and NAS boxes without a desktop. Use the app on computers people
sit at.

---

## How it behaves

- **The window closes to the tray.** The app and the agent keep running. **Quit Ourchive** in the
  tray stops both.
- **The tray shows contributor status.** The icon is grey when nothing is enrolled, green when every
  hub is connected, amber while reconnecting and red when a hub has given up. Hover it for details
  per hub.
- **Connection failures don't need a terminal.** Each hub retries 5 times with backoff, then waits.
  The app sends one notification, and **Retry connections** in the tray, or **Retry** in the window,
  starts it again. The app sends another notification when it recovers.
- **Start at login** in the tray registers the app to start hidden in the tray (`--background`).
- **Logs** are written to `client.log` next to the agent's log (see [agent.md](agent.md#agent-logs--retry-behavior)).
  Use **Show log file** in the tray to open the folder.

### Only one agent per computer

The CLI agent and the app share a per-user lock (`~/.config/ourchive/agent.lock`). If
`ourchive-agent` is already running as a service, the app still works for browsing but leaves
contributing to that service and says so on the Contribution page. To move contributing into the
app:

```bash
ourchive-agent service uninstall   # remove the login item
# stop the running agent (Task Manager / Activity Monitor / systemctl --user stop ourchive-agent)
```

Both use the same per-hub configs in `~/.config/ourchive/hubs/`, so nothing needs re-enrolling.

---

## Architecture

```text
┌──────────────────────────── Ourchive app (one process) ────────────────────────────┐
│                                                                                     │
│  Native window (WebView2 / WKWebView / WebKitGTK)                                    │
│   └─ bundled React UI (web/, built with --mode client)                               │
│        │ fetch("/api/…")         │ fetch("/_client/…")                               │
│        ▼                         ▼                                                   │
│  internal/clientcore  ── reverse proxy ──► active hub (session injected)             │
│        │                 bridge: hubs, browser sign-in, downloads, agent             │
│        ▼                                                                             │
│  internal/agentcore Runner ── WSS + yamux ──► each enrolled hub                      │
│                                                                                     │
│  client/ (Wails v3): tray, window, notifications, dialogs, ourchive:// links         │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

- **`internal/agentcore`** is the agent's engine: enrollment, the tunnel, and a `Runner` that keeps
  every hub connected, parks hubs that run out of retries, and publishes per-hub status
  (`internal/agentstatus`). It never touches stdin, stdout or `os.Exit`. Both `cmd/agent` and the
  app use it.
- **`internal/clientcore`** is the app's backend and doesn't depend on any GUI toolkit. It is an
  `http.Handler` placed in front of the bundled UI:
  - `/api/*` and `/downloads/*` are proxied to the **active hub**. The hub session is kept in
    `~/.config/ourchive/client/state.json` (mode `0600`) and added to each request. `Set-Cookie`
    responses update that file instead of reaching the webview.
  - `/_client/*` is a small JSON bridge: saved hubs, switching, browser sign-in, native save
    dialogs, agent retry and enrollment. The UI follows changes by long-polling
    `GET /_client/state?since=<version>`.
- **`client/`** is a separate Go module, so the hub and the headless agent still build with
  `CGO_ENABLED=0` and no GUI toolkit. It wires `clientcore` to Wails v3 and holds no other logic.
  The tray's labels and icons live in `client/internal/trayui` so they can be tested without a GUI.

### One UI, two builds

`web/src/platform.ts` is the only place the website and the app differ:

| | `npm run build` (hub website) | `npm run build:client` (app) |
| :--- | :--- | :--- |
| `isClient` | `false` | `true` |
| `hubOrigin()` | `window.location.origin` | the active hub's address |
| Downloads / external links | browser default | native save dialog / system browser |
| Hub switching | — (each hub is its own site) | top-bar switcher |
| Passkeys and SSO | in the page | "Sign in with your browser" (see below) |

Components call `isClient`, `hubOrigin()` and `openExternal()` instead of reading
`window.location` or the build mode. Client-only components live in
`web/src/components/ClientChrome.tsx`.

### Windows webview limitation

On Windows, Wails (WebView2) delivers a response to the page only after the Go handler has
finished, so responses can't stream there. Because of that:

- The app follows its own state by long-polling instead of server-sent events.
- The hub's live `/api/events` stream is refused with `501` on Windows, and the UI falls back to its
  15-second refresh. macOS and Linux stream normally.
- Video and audio previews are fetched in 8 MiB ranges (the proxy caps open-ended `Range`
  requests), so a long video isn't pulled into memory at once. Images and PDFs load whole.
  Downloads are unaffected: they go straight to disk from Go via the native save dialog.

Because the app bundles its own copy of the UI, it can be older or newer than a hub it talks to.
`/api/health` reports `api_version`. The app compares it with `clientcore.APIVersion` and shows a
banner when they differ. Bump `clientAPIVersion` in `cmd/hub/routes_client.go` only for breaking API
changes.

---

## Signing in

**Password** sign-in works inside the app. The request goes through the proxy, and the app keeps
the resulting session.

**Passkeys and SSO** are tied to the hub's web address, so they can't run inside the app's window.
**Sign in with your browser** uses a handoff instead:

1. The app creates a random PKCE verifier and opens
   `https://<hub>/?client_auth=<S256 challenge>&client_state=<state>` in the system browser.
2. The user signs in on the website with any method. The website keeps the request across SSO
   redirects and then asks **Continue to the app?**
3. The website calls `POST /api/auth/client/authorize`. The hub issues a one-time code (valid for
   2 minutes, bound to the challenge) and returns `ourchive://auth?code=…&state=…`, which the
   browser hands to the app.
4. The app checks that the state is one it issued, then calls `POST /api/auth/client/exchange`
   with the code and the verifier, and gets a new session of its own.

The code is burned on the first exchange attempt, and it is useless without the verifier, which
never leaves the app. Another program that registers `ourchive://` and intercepts the link gets
nothing.

---

## Contributing from the app

- **Contribution → This computer → Contribute this computer** enrolls the machine with the
  **active** hub. The page can't name another hub.
- **One-click from the website:** the hub's *Connect a machine* dialog has **Open in Ourchive app**,
  an `ourchive://enroll?hub=…&token=…` link. The app never acts on that link by itself. It shows the
  hub's address and asks for confirmation first, because a hub this computer contributes to can ask
  for local folders to be shared. Only accept links you started yourself.

---

## Mobile

Phones suspend background apps, so they can't keep a contributor tunnel open. Mobile builds compile
the client without the agent (`contributor_mobile.go`). The UI then hides contributing and says why.
The rest (browse, preview, upload, download, share, several hubs) is the same code. Wails v3's
mobile support is still experimental, so treat mobile builds as previews.

---

## Getting the app from the hub website

The hub's **Contribution** page has an **Ourchive desktop app** card, and the *Connect a contributor
machine* dialog has the same control. **Open Ourchive app** hands an `ourchive://` link to the
browser, which opens the app if the OS has it registered. If the page is still in front a moment
later, the app probably isn't installed, so **Download for \<your OS\>** is highlighted.

Two limits come from the browser, not from Ourchive: a web page can't tell whether an app is
installed (it only guesses from whether the page lost focus), and it can't run a downloaded file, so
the user runs the installer themselves. Unsigned builds also trigger SmartScreen on Windows and
Gatekeeper on macOS until the installers are code-signed.

The hub offers installers from the first of these that has any:

1. **A GitHub release.** Set `OURCHIVE_CLIENT_RELEASE_REPO=owner/repo`. The hub offers the newest
   *published* (not draft, not pre-release) release whose tag starts with `client-v`, caches the
   lookup for 10 minutes and keeps serving the last answer if GitHub is unreachable. Only files under
   that repository's releases are ever offered.
2. **Files on the hub.** Put installers in `OURCHIVE_DIST_DIR` (default `dist/`) with the names
   below. The hub serves them from `/downloads/`, like the agent binaries.

| System | File name |
| --- | --- |
| Windows | `ourchive-client-windows-amd64.exe` |
| macOS (Apple silicon) | `ourchive-client-macos-arm64.zip` |
| Linux | `ourchive-client-linux-amd64.AppImage` |

With neither, the card says the hub doesn't host the app yet.

### Publishing a release

Push a tag such as `client-v0.1.0`. The **Build Ourchive App** workflow builds all three installers
with the names above and attaches them to a **draft** release. Review it, then publish it. Untick
**Set as the latest release** when you do: the agent's install commands use `releases/latest`, and a
client-only release has no agent files.

### Trying it without a release

Build the app (see below), copy the installer from `client/bin/` into the hub's `dist/` folder under
the name from the table, and open the Contribution page on a hub started from that folder. The card
then offers that file as the download.

## Building

Prerequisites: Go 1.26, Node 22, and the [Wails v3 platform dependencies](https://v3.wails.io/getting-started/installation)
(WebView2 on Windows, Xcode command line tools on macOS, `libgtk-3-dev` and `libwebkit2gtk-4.1-dev` on
Linux).

```bash
go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0

cd client
./scripts/bootstrap-build-assets.sh   # once: generates build/ (installers, Info.plist, icons, ourchive://)
go mod tidy                           # once: writes client/go.sum

wails3 task build                     # builds ../web in client mode, then the app → bin/ourchive
wails3 task package                   # installer / .app / AppImage for the current OS
wails3 task dev                       # live reload against the Vite dev server
```

Product metadata and the `ourchive://` scheme are set in `client/build/config.ourchive.yml`. After
changing that file, rerun `wails3 task common:update:build-assets`.

Production Windows builds use `-H windowsgui`, so the app never opens a console window.

### Tests

```bash
go test ./internal/agentcore ./internal/agentstatus ./internal/clientcore ./internal/handoff ./cmd/hub
(cd client && go test ./internal/...)
(cd web && npx tsc --noEmit)
```

/**
 * ClientChrome — UI that only exists in the desktop-client build: the hub switcher, the
 * first-run "add a hub" screen, the contribution confirmation for ourchive://enroll links, the
 * "This computer" contributor card, and client status banners.
 *
 * Everything here reads `useClientState()` and talks to the app through `clientBridge`; the web
 * build never renders these components.
 */
import { FormEvent, ReactNode, useEffect, useMemo, useRef, useState } from "react";
import { Check, ChevronDown, Globe, Monitor, Plus, RefreshCw, X } from "lucide-react";
import { api } from "../api";
import {
  AgentHubStatus,
  ClientHub,
  ClientState,
  activeClientHub,
  clientBridge,
  useClientState,
} from "../platform";
import { ModalDialog } from "./ModalDialog";
import { StickyMessage } from "./StickyBannerStack";

function errorText(err: unknown, fallback: string) {
  return err instanceof Error ? err.message : fallback;
}

// ---- Root ----

/**
 * ClientRoot decides what the app window shows: the first-run screen, or the regular App for the
 * active hub. The App is re-mounted whenever the active hub changes or a browser sign-in
 * completes, so every view starts clean against the new hub.
 */
export function ClientRoot({ renderApp }: { renderApp: (key: string) => ReactNode }) {
  const st = useClientState();
  const hub = st ? st.hubs.find((h) => h.id === st.active_hub_id) : undefined;
  const signInEpoch = st?.sign_in && !st.sign_in.pending && !st.sign_in.error && st.sign_in.hub_id === hub?.id ? st.sign_in.at : "";

  if (!st) {
    return (
      <div className="auth-stage">
        <div className="auth-card card">
          <p className="page-sub">Starting Ourchive…</p>
        </div>
      </div>
    );
  }
  return (
    <>
      {hub ? renderApp(`${hub.id}:${signInEpoch}`) : <ClientWelcome />}
      <EnrollmentPrompt state={st} />
    </>
  );
}

// ---- Hub switcher ----

export function HubSwitcher() {
  const st = useClientState();
  const [open, setOpen] = useState(false);
  const [adding, setAdding] = useState(false);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const wrapRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    function onDoc(e: MouseEvent) {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) setOpen(false);
    }
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") setOpen(false);
    }
    document.addEventListener("mousedown", onDoc);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDoc);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  if (!st) return null;
  const active = activeClientHub();

  async function run(id: string, fn: () => Promise<unknown>) {
    setError(null);
    setBusyId(id);
    try {
      await fn();
    } catch (err) {
      setError(errorText(err, "Something went wrong"));
    } finally {
      setBusyId(null);
    }
  }

  return (
    <div className="hub-switcher-wrap" ref={wrapRef}>
      <button type="button" className="btn" aria-haspopup="dialog" aria-expanded={open} onClick={() => setOpen((v) => !v)}>
        <Globe size={14} strokeWidth={1.6} />
        <span>{active?.name ?? "Choose a hub"}</span>
        <ChevronDown size={13} strokeWidth={1.6} />
      </button>

      {open && (
        <div className="hub-switcher card" role="dialog" aria-label="Hubs">
          {st.hubs.map((h) => (
            <div key={h.id} className="hub-row" aria-current={h.active ? "true" : undefined}>
              <button
                type="button"
                className="hub-row-main"
                disabled={busyId !== null}
                onClick={() =>
                  h.active
                    ? setOpen(false)
                    : void run(h.id, async () => {
                        await clientBridge.activateHub(h.id);
                        setOpen(false);
                      })
                }
              >
                <span className={`dot ${h.info && !h.info.reachable ? "offline" : h.signed_in ? "" : "paused"}`} />
                <span className="hub-row-text">
                  <span className="hub-row-name">{h.name}</span>
                  <span className="hub-row-url">
                    {h.url.replace(/^https?:\/\//, "")}
                    {h.signed_in ? "" : " · signed out"}
                  </span>
                </span>
                {h.active && <Check size={14} strokeWidth={1.8} />}
              </button>
              <button
                type="button"
                className="hub-remove-btn"
                title={`Remove ${h.name}`}
                aria-label={`Remove hub ${h.name}`}
                disabled={busyId !== null}
                onClick={() => {
                  if (!window.confirm(`Remove ${h.name} from this app? You can add it again later.`)) return;
                  void run(h.id, () => clientBridge.removeHub(h.id));
                }}
              >
                <X size={11} />
              </button>
            </div>
          ))}
          {error && (
            <div className="alert-error" role="alert">
              {error}
            </div>
          )}
          <button type="button" className="btn btn-sm" onClick={() => setAdding(true)}>
            <Plus size={13} strokeWidth={1.6} />
            <span>Add hub</span>
          </button>
        </div>
      )}

      <ModalDialog open={adding} title="Add an Ourchive hub" onClose={() => setAdding(false)}>
        <AddHubForm
          onDone={() => {
            setAdding(false);
            setOpen(false);
          }}
          onCancel={() => setAdding(false)}
        />
      </ModalDialog>
    </div>
  );
}

function AddHubForm({ onDone, onCancel }: { onDone: () => void; onCancel?: () => void }) {
  const [url, setUrl] = useState("");
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [canForce, setCanForce] = useState(false);

  async function submit(e: FormEvent, force = false) {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      await clientBridge.addHub(url, name, force);
      onDone();
    } catch (err) {
      setError(errorText(err, "Could not add hub"));
      setCanForce(true);
    } finally {
      setBusy(false);
    }
  }

  return (
    <form className="form-stack" onSubmit={(e) => void submit(e)}>
      <div className="field">
        <label htmlFor="client-hub-url">Hub address</label>
        <span id="client-hub-url-hint" className="field-hint">
          The address you use to open your group&apos;s Ourchive in a browser.
        </span>
        <input
          id="client-hub-url"
          type="text"
          className="mono"
          required
          autoFocus
          spellCheck={false}
          aria-describedby="client-hub-url-hint"
          placeholder="archive.example.com"
          value={url}
          onChange={(e) => {
            setUrl(e.target.value);
            setCanForce(false);
          }}
        />
      </div>
      <div className="field">
        <label htmlFor="client-hub-name">Label (optional)</label>
        <input
          id="client-hub-name"
          type="text"
          placeholder="Defaults to the hub's own name"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
      </div>
      {error && (
        <div className="alert-error" role="alert">
          {error}
        </div>
      )}
      <div className="modal-actions">
        {onCancel && (
          <button type="button" className="btn" onClick={onCancel}>
            Cancel
          </button>
        )}
        {canForce && (
          <button type="button" className="btn" disabled={busy} onClick={(e) => void submit(e, true)}>
            Add anyway
          </button>
        )}
        <button type="submit" className="btn btn-primary" disabled={busy}>
          {busy ? "Checking…" : "Add hub"}
        </button>
      </div>
    </form>
  );
}

// ---- First run ----

function ClientWelcome() {
  return (
    <div className="auth-stage">
      <div className="auth-card card">
        <div className="auth-brand">
          <span className="brand-mark" aria-hidden="true">
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
              <path d="M4 7h16M4 12h16M4 17h10" />
            </svg>
          </span>
          <span>ourchive</span>
        </div>
        <h1 className="page-title" style={{ marginTop: 16 }}>
          Connect to your archive
        </h1>
        <p className="page-sub" style={{ marginBottom: 24 }}>
          Add the hub your group uses. You can add more hubs later and switch between them from the top bar.
        </p>
        <AddHubForm onDone={() => {}} />
      </div>
    </div>
  );
}

// ---- Browser sign-in (AuthView in the client build) ----

/** Replaces passkey and SSO buttons, which only work on the hub's own website. */
export function BrowserSignInButton({ hasSSO }: { hasSSO: boolean }) {
  const st = useClientState();
  const hub = activeClientHub();
  const [error, setError] = useState<string | null>(null);
  if (!hub) return null;
  const pending = st?.sign_in?.pending && st.sign_in.hub_id === hub.id;
  const failed = st?.sign_in && !st.sign_in.pending && st.sign_in.hub_id === hub.id ? st.sign_in.error : undefined;

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
      <button
        type="button"
        className="btn"
        style={{ height: 36, justifyContent: "center" }}
        onClick={() => {
          setError(null);
          clientBridge.signInWithBrowser(hub.id).catch((err) => setError(errorText(err, "Could not open your browser")));
        }}
      >
        <Globe size={14} strokeWidth={1.6} />
        <span>{hasSSO ? "Use a passkey or SSO in your browser" : "Sign in with your browser"}</span>
      </button>
      {pending && <span className="field-hint">Finish signing in in your browser, then come back here.</span>}
      {(error || failed) && (
        <div className="alert-error" role="alert">
          {error || failed}
        </div>
      )}
    </div>
  );
}

// ---- ourchive://enroll confirmation ----

function EnrollmentPrompt({ state }: { state: ClientState }) {
  const pending = state.pending_enrollment;
  const [name, setName] = useState("");
  const [readOnly, setReadOnly] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function answer(accept: boolean) {
    setError(null);
    setBusy(true);
    try {
      await clientBridge.answerEnrollment(accept, name, readOnly);
    } catch (err) {
      setError(errorText(err, "Could not connect this computer"));
    } finally {
      setBusy(false);
    }
  }

  return (
    <ModalDialog open={Boolean(pending)} title="Contribute this computer?" onClose={() => void answer(false)}>
      {pending && (
        <div className="form-stack">
          <p className="page-sub" style={{ marginTop: 0 }}>
            <strong className="mono">{pending.hub_url}</strong> asked to connect this computer as a contributor
            {pending.hub_saved ? "." : ". This hub isn't in your list yet; it will be added."}
          </p>
          <p className="page-sub" style={{ marginTop: 0 }}>
            Contributing lets you share folders from this computer through that hub. Only continue if you started
            this from a hub you trust.
          </p>
          <div className="field">
            <label htmlFor="enroll-prompt-name">Machine name (optional)</label>
            <input
              id="enroll-prompt-name"
              type="text"
              placeholder="Defaults to this computer's name"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </div>
          <label style={{ display: "flex", gap: 8, alignItems: "center" }}>
            <input type="checkbox" checked={readOnly} onChange={(e) => setReadOnly(e.target.checked)} />
            <span>Read-only: never accept uploads, renames or deletes on this computer</span>
          </label>
          {error && (
            <div className="alert-error" role="alert">
              {error}
            </div>
          )}
          <div className="modal-actions">
            <button type="button" className="btn" disabled={busy} onClick={() => void answer(false)}>
              Not now
            </button>
            <button type="button" className="btn btn-primary" disabled={busy} onClick={() => void answer(true)}>
              {busy ? "Connecting…" : "Contribute this computer"}
            </button>
          </div>
        </div>
      )}
    </ModalDialog>
  );
}

// ---- Contribution: this computer ----

function describeAgent(h: AgentHubStatus): string {
  switch (h.state) {
    case "connected":
      return "Connected";
    case "connecting":
      return "Connecting…";
    case "retrying":
      return `Reconnecting (attempt ${h.failures} of ${h.max_retries || "∞"})`;
    case "failed":
      return "Offline: gave up after repeated failures";
    default:
      return "Starting…";
  }
}

/** Shown at the top of Contribution in the client: this machine's status for the active hub. */
export function ThisComputerCard({ onEnrolled }: { onEnrolled: () => void }) {
  const st = useClientState();
  const hub = activeClientHub();
  const [name, setName] = useState("");
  const [readOnly, setReadOnly] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  if (!st || !hub) return null;

  if (!st.platform.contributor) {
    return (
      <section className="card" style={{ padding: "var(--sp-4)" }}>
        <div style={{ display: "flex", gap: 10, alignItems: "center" }}>
          <Monitor size={18} strokeWidth={1.5} />
          <strong>This computer</strong>
        </div>
        <p className="page-sub" style={{ marginBottom: 0 }}>
          {st.platform.contributor_note || "This device can't contribute storage."}
        </p>
      </section>
    );
  }

  const status = st.agent.find((a) => a.hub_url === hub.url);

  async function contribute(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      const tok = await api.createEnrollToken(name);
      await clientBridge.enrollActiveHub(tok.token, name, readOnly);
      onEnrolled();
    } catch (err) {
      setError(errorText(err, "Could not connect this computer"));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="card" style={{ padding: "var(--sp-4)", display: "flex", flexDirection: "column", gap: 12 }}>
      <div style={{ display: "flex", gap: 10, alignItems: "center" }}>
        <Monitor size={18} strokeWidth={1.5} />
        <strong>This computer</strong>
      </div>
      {status ? (
        <div className="agent-status-row">
          <span style={{ display: "flex", gap: 8, alignItems: "center" }}>
            <span className={`dot ${status.state === "connected" ? "" : status.state === "failed" ? "offline" : "paused"}`} />
            <span>
              {status.display_name ? `${status.display_name}: ` : ""}
              {describeAgent(status)}
              {status.last_error && status.state !== "connected" ? ` (${status.last_error})` : ""}
            </span>
          </span>
          {status.state !== "connected" && (
            <button type="button" className="btn btn-sm" onClick={() => void clientBridge.retryAgent(status.hub_url)}>
              <RefreshCw size={13} strokeWidth={1.6} />
              <span>Retry now</span>
            </button>
          )}
        </div>
      ) : (
        <form className="form-stack" onSubmit={(e) => void contribute(e)}>
          <p className="page-sub" style={{ margin: 0 }}>
            Share folders from this computer with {hub.name}. The app keeps the connection running in the background,
            even with this window closed.
          </p>
          <div className="field">
            <label htmlFor="this-computer-name">Machine name (optional)</label>
            <input
              id="this-computer-name"
              type="text"
              placeholder="Defaults to this computer's name"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </div>
          <label style={{ display: "flex", gap: 8, alignItems: "center" }}>
            <input type="checkbox" checked={readOnly} onChange={(e) => setReadOnly(e.target.checked)} />
            <span>Read-only: never accept uploads, renames or deletes on this computer</span>
          </label>
          {error && (
            <div className="alert-error" role="alert">
              {error}
            </div>
          )}
          <div>
            <button type="submit" className="btn btn-primary" disabled={busy}>
              <Plus size={15} strokeWidth={1.6} />
              <span>{busy ? "Connecting…" : "Contribute this computer"}</span>
            </button>
          </div>
        </form>
      )}
    </section>
  );
}

// ---- Banners ----

/** Client-only banners, merged into App's sticky banner stack. */
export function useClientStickyMessages(): StickyMessage[] {
  const st = useClientState();
  return useMemo(() => {
    if (!st) return [];
    const out: StickyMessage[] = [];
    const hub: ClientHub | undefined = st.hubs.find((h) => h.id === st.active_hub_id);
    if (hub?.info?.reachable && !hub.info.compatible && hub.info.error) {
      out.push({ id: `client-compat-${hub.id}`, kind: "warning", title: "Version mismatch", body: hub.info.error, dismissible: true });
    }
    const failed = st.agent.filter((a) => a.state === "failed");
    if (failed.length > 0) {
      out.push({
        id: "client-agent-failed",
        kind: "error",
        title: "This computer stopped contributing",
        body: `Couldn't reach ${failed.map((f) => f.hub_url.replace(/^https?:\/\//, "")).join(", ")} after repeated attempts.`,
        dismissible: false,
        actionLabel: "Retry",
        onAction: () => void clientBridge.retryAgent(""),
      });
    }
    return out;
  }, [st]);
}

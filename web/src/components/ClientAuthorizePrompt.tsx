/**
 * ClientAuthorizePrompt — the hub website's half of the desktop app's browser sign-in.
 *
 * The app opens this hub with `?client_auth=…&client_state=…`. Once the user is signed in here
 * (by any method), this asks them to confirm, then hands a one-time code back to the app through
 * an ourchive:// link. Asking first means a link from somewhere else can't silently sign an app in.
 */
import { useState } from "react";
import { Monitor } from "lucide-react";
import { api, User } from "../api";
import { PendingClientAuth, clearPendingClientAuth, hubHost } from "../platform";
import { ModalDialog } from "./ModalDialog";

export function ClientAuthorizePrompt({ user, pending, onDone }: { user: User; pending: PendingClientAuth; onDone: () => void }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [sent, setSent] = useState(false);

  function close() {
    clearPendingClientAuth();
    onDone();
  }

  async function approve() {
    setError(null);
    setBusy(true);
    try {
      const res = await api.authorizeClient(pending.challenge, pending.state);
      clearPendingClientAuth();
      setSent(true);
      window.location.href = res.redirect_url;
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not sign in the app");
    } finally {
      setBusy(false);
    }
  }

  return (
    <ModalDialog open title="Sign in to the Ourchive app" onClose={close}>
      <div className="form-stack">
        <div style={{ display: "flex", gap: 10, alignItems: "center" }}>
          <Monitor size={18} strokeWidth={1.5} />
          <strong>
            {user.display_name} on {hubHost()}
          </strong>
        </div>
        {sent ? (
          <p className="page-sub" style={{ marginTop: 0 }}>
            Your browser should hand you back to the Ourchive app now. You can close this tab.
          </p>
        ) : (
          <p className="page-sub" style={{ marginTop: 0 }}>
            The Ourchive desktop app on this computer asked to sign in to this hub as you. Continue only if you just
            started this from the app.
          </p>
        )}
        {error && (
          <div className="alert-error" role="alert">
            {error}
          </div>
        )}
        <div className="modal-actions">
          <button type="button" className="btn" onClick={close}>
            {sent ? "Close" : "Cancel"}
          </button>
          {!sent && (
            <button type="button" className="btn btn-primary" disabled={busy} onClick={() => void approve()}>
              {busy ? "Signing in…" : "Continue to the app"}
            </button>
          )}
        </div>
      </div>
    </ModalDialog>
  );
}

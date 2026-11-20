/**
 * UserSettingsFlyout — Top-right user settings popover containing account identity,
 * theme switcher, animated gradient button toggle, transfer mode readout, password rotation,
 * sign-out-all-devices, and sign-out.
 */
import { FormEvent, useEffect, useRef, useState } from "react";
import { Check, ChevronDown, KeyRound, LogOut, Radio, Settings } from "lucide-react";
import { api, PublicNetworkConfig, User } from "../api";
import { ModalDialog } from "./ModalDialog";

interface UserSettingsFlyoutProps {
  user: User;
  scheme: "dark" | "light";
  onChangeScheme: (next: "dark" | "light") => void;
  btnMotion: boolean;
  onChangeBtnMotion: (next: boolean) => void;
  netConfig: PublicNetworkConfig | null;
  onLogout: () => Promise<void>;
}

export function UserSettingsFlyout({
  user,
  scheme,
  onChangeScheme,
  btnMotion,
  onChangeBtnMotion,
  netConfig,
  onLogout,
}: UserSettingsFlyoutProps) {
  const [open, setOpen] = useState(false);
  const [pwOpen, setPwOpen] = useState(false);
  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [pwBusy, setPwBusy] = useState(false);
  const [pwError, setPwError] = useState<string | null>(null);
  const [pwDone, setPwDone] = useState(false);
  const wrapRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    function onDocClick(e: MouseEvent) {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    }
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") setOpen(false);
    }
    document.addEventListener("mousedown", onDocClick);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDocClick);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  async function handleChangePassword(e: FormEvent) {
    e.preventDefault();
    setPwError(null);
    setPwBusy(true);
    try {
      await api.changePassword({ current_password: currentPassword, new_password: newPassword });
      setCurrentPassword("");
      setNewPassword("");
      setPwDone(true);
      window.setTimeout(() => {
        setPwOpen(false);
        setPwDone(false);
      }, 1200);
    } catch (err) {
      setPwError(err instanceof Error ? err.message : "Failed to change password");
    } finally {
      setPwBusy(false);
    }
  }

  async function handleLogoutAll() {
    if (!window.confirm("Sign out of all browsers and devices?")) return;
    setOpen(false);
    await api.logoutAll();
    await onLogout();
  }

  return (
    <div className="user-flyout-wrap" ref={wrapRef}>
      <button
        type="button"
        className="btn"
        aria-haspopup="dialog"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <Settings size={14} strokeWidth={1.6} />
        <span>{user.display_name}</span>
        <ChevronDown size={13} strokeWidth={1.6} />
      </button>

      {open && (
        <div className="user-flyout card" role="dialog" aria-label="User settings">
          <div className="user-flyout-head">
            <div>
              <strong>{user.display_name}</strong>
              <div className="inline-muted mono" style={{ fontSize: 12 }}>
                {user.username ? `@${user.username}` : user.email || "Member"}
              </div>
            </div>
            <span className="tag-chip mono">{user.role}</span>
          </div>

          <div className="user-flyout-section">
            <div className="cap-row">
              <div>
                <strong>Theme</strong>
                <small>Warm Paper palette</small>
              </div>
              <div className="segmented" role="group" aria-label="Theme preference">
                <button
                  type="button"
                  className="seg-btn"
                  aria-pressed={scheme === "dark"}
                  onClick={() => onChangeScheme("dark")}
                >
                  Dark
                </button>
                <button
                  type="button"
                  className="seg-btn"
                  aria-pressed={scheme === "light"}
                  onClick={() => onChangeScheme("light")}
                >
                  Light
                </button>
              </div>
            </div>

            <label className="switch-row">
              <span>
                <strong>Animated primary buttons</strong>
                <small>Subtle iridescent gradient drift</small>
              </span>
              <input
                type="checkbox"
                checked={btnMotion}
                onChange={(e) => onChangeBtnMotion(e.target.checked)}
                {...({ switch: "" } as Record<string, string>)}
              />
            </label>

            <div className="user-flyout-meta">
              <Radio size={13} strokeWidth={1.6} />
              <span>
                {netConfig?.p2p_enabled
                  ? netConfig.allow_relay_fallback
                    ? "WebRTC P2P (with Hub fallback)"
                    : "WebRTC P2P direct only"
                  : "Hub relay streaming"}
              </span>
            </div>
          </div>

          <div className="user-flyout-foot" style={{ display: "flex", flexDirection: "column", gap: 6 }}>
            <button
              type="button"
              className="btn"
              style={{ width: "100%" }}
              onClick={() => {
                setOpen(false);
                setPwError(null);
                setPwDone(false);
                setPwOpen(true);
              }}
            >
              <KeyRound size={14} strokeWidth={1.6} />
              <span>Change password</span>
            </button>
            <button type="button" className="btn" style={{ width: "100%" }} onClick={() => void handleLogoutAll()}>
              <LogOut size={14} strokeWidth={1.6} />
              <span>Sign out all devices</span>
            </button>
            <button
              type="button"
              className="btn btn-danger"
              style={{ width: "100%" }}
              onClick={() => {
                setOpen(false);
                void onLogout();
              }}
            >
              <LogOut size={14} strokeWidth={1.6} />
              <span>Sign out</span>
            </button>
          </div>
        </div>
      )}

      <ModalDialog open={pwOpen} title="Change password" onClose={() => setPwOpen(false)}>
        <form className="form-stack" onSubmit={handleChangePassword}>
          <p className="page-sub" style={{ marginTop: 0 }}>
            Changing your password automatically signs out all other active sessions.
          </p>
          <div className="field">
            <label htmlFor="pw-current">Current password</label>
            <input
              id="pw-current"
              type="password"
              autoComplete="current-password"
              required
              value={currentPassword}
              onChange={(e) => setCurrentPassword(e.target.value)}
            />
          </div>
          <div className="field">
            <label htmlFor="pw-new">New password</label>
            <span className="field-hint">At least 8 characters.</span>
            <input
              id="pw-new"
              type="password"
              autoComplete="new-password"
              required
              minLength={8}
              value={newPassword}
              onChange={(e) => setNewPassword(e.target.value)}
            />
          </div>
          {pwError && (
            <div className="alert-error" role="alert">
              {pwError}
            </div>
          )}
          {pwDone && (
            <div className="status-banner" role="status">
              <Check size={14} strokeWidth={1.8} />
              <span>Password updated. Other sessions signed out.</span>
            </div>
          )}
          <div className="modal-actions">
            <button type="button" className="btn" onClick={() => setPwOpen(false)}>
              Cancel
            </button>
            <button type="submit" className="btn btn-primary" disabled={pwBusy}>
              {pwBusy ? "Updating…" : "Update password"}
            </button>
          </div>
        </form>
      </ModalDialog>
    </div>
  );
}

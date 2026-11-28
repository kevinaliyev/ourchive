/**
 * UserSettingsFlyout — Top-right user settings popover containing account identity,
 * theme switcher, animated gradient button toggle, transfer mode readout, password rotation,
 * sign-out-all-devices, and sign-out.
 */
import { FormEvent, useCallback, useEffect, useRef, useState } from "react";
import { Check, ChevronDown, Fingerprint, KeyRound, LogOut, Pencil, Plus, Radio, Settings, Trash2 } from "lucide-react";
import { api, PublicNetworkConfig, User } from "../api";
import {
  getPasskeyCapabilities,
  registerNewPasskey,
  StoredPasskeyCredential,
  syncAcceptedCredentials,
  syncCurrentUserDetails,
} from "../passkeys";
import { hubOrigin, isClient, openExternal } from "../platform";
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

  const [pkOpen, setPkOpen] = useState(false);
  const [passkeys, setPasskeys] = useState<StoredPasskeyCredential[]>([]);
  const [pkCanCreate, setPkCanCreate] = useState(false);
  const [pkBusy, setPkBusy] = useState(false);
  const [pkError, setPkError] = useState<string | null>(null);
  const [pkNewName, setPkNewName] = useState("");
  const [profileDisplay, setProfileDisplay] = useState(user.display_name);
  const [profileUsername, setProfileUsername] = useState(user.username ?? "");
  const [profileSaved, setProfileSaved] = useState(false);
  const wrapRef = useRef<HTMLDivElement>(null);

  const loadPasskeys = useCallback(async () => {
    try {
      const [res, caps] = await Promise.all([api.listPasskeys(), getPasskeyCapabilities()]);
      setPasskeys(res.credentials);
      setPkCanCreate(Boolean(caps.supported && (caps.platformAuthenticator || caps.conditionalGet)));
      if (res.rpId && res.userId) {
        void syncAcceptedCredentials(res.rpId, res.userId, res.credentials);
      }
    } catch (err) {
      setPkError(err instanceof Error ? err.message : "Failed to load passkeys");
    }
  }, []);

  useEffect(() => {
    if (pkOpen) {
      setProfileDisplay(user.display_name);
      setProfileUsername(user.username ?? "");
      setProfileSaved(false);
      void loadPasskeys();
    }
  }, [pkOpen, user.display_name, user.username, loadPasskeys]);

  async function handleCreatePasskey() {
    setPkError(null);
    setPkBusy(true);
    try {
      const created = await registerNewPasskey(pkNewName.trim(), false);
      if (created) {
        setPkNewName("");
        await loadPasskeys();
      }
    } catch (err) {
      setPkError(err instanceof Error ? err.message : "Failed to register passkey");
    } finally {
      setPkBusy(false);
    }
  }

  async function handleRenamePasskey(cred: StoredPasskeyCredential) {
    const next = window.prompt("Rename passkey:", cred.name);
    if (!next || !next.trim() || next.trim() === cred.name) return;
    try {
      await api.renamePasskey(cred.id, next.trim());
      await loadPasskeys();
    } catch (err) {
      setPkError(err instanceof Error ? err.message : "Rename failed");
    }
  }

  async function handleDeletePasskey(cred: StoredPasskeyCredential) {
    if (!window.confirm(`Remove passkey "${cred.name}"?`)) return;
    try {
      await api.deletePasskey(cred.id);
      await loadPasskeys();
    } catch (err) {
      setPkError(err instanceof Error ? err.message : "Delete failed");
    }
  }

  async function handleSaveProfile(e: FormEvent) {
    e.preventDefault();
    setPkError(null);
    setProfileSaved(false);
    setPkBusy(true);
    try {
      const res = await api.updateProfile({
        username: profileUsername.trim(),
        display_name: profileDisplay.trim(),
      });
      if (res.rpId && res.userId) {
        const acctName = res.user.username || res.user.email || res.user.display_name;
        void syncCurrentUserDetails(res.rpId, res.userId, acctName, res.user.display_name);
      }
      setProfileSaved(true);
    } catch (err) {
      setPkError(err instanceof Error ? err.message : "Profile update failed");
    } finally {
      setPkBusy(false);
    }
  }

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
                setPkError(null);
                setPkOpen(true);
              }}
            >
              <Fingerprint size={14} strokeWidth={1.6} />
              <span>Passkeys &amp; Profile</span>
            </button>
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

      <ModalDialog open={pkOpen} title="Passkeys & Profile" onClose={() => setPkOpen(false)}>
        <div className="form-stack">
          <form className="form-stack" onSubmit={handleSaveProfile}>
            <div className="field">
              <label htmlFor="prof-display">Display name</label>
              <input
                id="prof-display"
                type="text"
                required
                value={profileDisplay}
                onChange={(e) => setProfileDisplay(e.target.value)}
              />
            </div>
            <div className="field">
              <label htmlFor="prof-user">Username</label>
              <input
                id="prof-user"
                type="text"
                value={profileUsername}
                onChange={(e) => setProfileUsername(e.target.value)}
                placeholder="e.g. ru"
              />
            </div>
            <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
              <button type="submit" className="btn" disabled={pkBusy}>
                Save profile
              </button>
              {profileSaved && <span className="inline-muted">Saved &amp; synced</span>}
            </div>
          </form>

          <hr style={{ border: "none", borderTop: "1px solid var(--border)", margin: "6px 0" }} />

          <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 8 }}>
            <strong>Registered Passkeys</strong>
            {pkCanCreate && (
              <div style={{ display: "flex", gap: 6 }}>
                <input
                  type="text"
                  value={pkNewName}
                  onChange={(e) => setPkNewName(e.target.value)}
                  placeholder="Label (optional)"
                  style={{ width: 130, height: 32 }}
                />
                <button
                  type="button"
                  className="btn btn-primary"
                  disabled={pkBusy}
                  onClick={() => void handleCreatePasskey()}
                >
                  <Plus size={14} strokeWidth={1.6} />
                  <span>Create passkey</span>
                </button>
              </div>
            )}
          </div>

          {isClient && (
            <div className="user-flyout-meta" style={{ justifyContent: "space-between" }}>
              <span>Passkeys belong to the hub&apos;s website. Add or remove them there.</span>
              <button type="button" className="btn btn-sm" onClick={() => openExternal(hubOrigin())}>
                Open in browser
              </button>
            </div>
          )}

          {passkeys.length === 0 ? (
            <p className="inline-muted" style={{ margin: 0 }}>
              No passkeys registered yet.
            </p>
          ) : (
            <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
              {passkeys.map((cred) => (
                <div
                  key={cred.id}
                  style={{
                    display: "flex",
                    alignItems: "center",
                    justifyContent: "space-between",
                    gap: 8,
                    padding: "8px 10px",
                    border: "1px solid var(--border)",
                    borderRadius: 8,
                  }}
                >
                  <div style={{ minWidth: 0 }}>
                    <div style={{ fontWeight: 500, display: "flex", alignItems: "center", gap: 6 }}>
                      <Fingerprint size={14} strokeWidth={1.6} />
                      <span>{cred.name}</span>
                      {cred.credentialBackedUp && <span className="tag-chip">Synced</span>}
                    </div>
                    <div className="inline-muted" style={{ fontSize: 12 }}>
                      Added {new Date(cred.registeredAt).toLocaleDateString()}
                      {cred.lastUsedAt ? ` · Last used ${new Date(cred.lastUsedAt).toLocaleDateString()}` : ""}
                    </div>
                  </div>
                  <div style={{ display: "flex", gap: 4 }}>
                    <button
                      type="button"
                      className="btn btn-ghost"
                      title="Rename passkey"
                      onClick={() => void handleRenamePasskey(cred)}
                    >
                      <Pencil size={13} strokeWidth={1.6} />
                    </button>
                    <button
                      type="button"
                      className="btn btn-ghost"
                      title="Delete passkey"
                      onClick={() => void handleDeletePasskey(cred)}
                    >
                      <Trash2 size={13} strokeWidth={1.6} />
                    </button>
                  </div>
                </div>
              ))}
            </div>
          )}

          {pkError && (
            <div className="alert-error" role="alert">
              {pkError}
            </div>
          )}
        </div>
      </ModalDialog>

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

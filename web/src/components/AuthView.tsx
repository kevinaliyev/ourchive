/**
 * AuthView — First-run Host Admin setup, email/username sign-in, and invite-only registration.
 * Dynamically adapts required identifier fields to the Hub's `auth_identity_mode` policy
 * and automatically prefills invite keys from `?invite=` deep links.
 */
import { FormEvent, useEffect, useRef, useState } from "react";
import { Eye, EyeOff, Fingerprint, Globe, KeyRound } from "lucide-react";
import { api, AuthIdentityMode, User } from "../api";
import {
  getPasskeyCapabilities,
  startConditionalPasskeyLogin,
  triggerButtonPasskeyLogin,
} from "../passkeys";

interface AuthViewProps {
  initialized: boolean;
  identityMode: AuthIdentityMode;
  onAuthenticated: (user: User) => void;
}

export function AuthView({ initialized, identityMode, onAuthenticated }: AuthViewProps) {
  const [mode, setMode] = useState<"login" | "invite">(() => {
    try {
      const p = new URLSearchParams(window.location.search);
      return p.get("invite") ? "invite" : "login";
    } catch {
      return "login";
    }
  });

  const [inviteCode, setInviteCode] = useState(() => {
    try {
      return new URLSearchParams(window.location.search).get("invite") ?? "";
    } catch {
      return "";
    }
  });
  const [identifier, setIdentifier] = useState("");
  const [email, setEmail] = useState("");
  const [username, setUsername] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [password, setPassword] = useState("");
  const [showPassword, setShowPassword] = useState(false);
  const [error, setError] = useState<string | null>(() => {
    try {
      return new URLSearchParams(window.location.search).get("auth_error");
    } catch {
      return null;
    }
  });
  const [busy, setBusy] = useState(false);
  const [passkeyBusy, setPasskeyBusy] = useState(false);
  const [passkeysAvailable, setPasskeysAvailable] = useState(false);
  const [oidcProviders, setOidcProviders] = useState<{ id: string; name: string }[]>([]);
  const abortConditionalRef = useRef<(() => void) | null>(null);

  useEffect(() => {
    if (!initialized) {
      setMode("login");
      return;
    }
    let active = true;
    Promise.all([api.listOIDCProviders().catch(() => ({ providers: [], passkeys_enabled: false })), getPasskeyCapabilities()]).then(
      ([srv, caps]) => {
        if (!active) return;
        setOidcProviders(srv.providers || []);
        setPasskeysAvailable(Boolean(srv.passkeys_enabled && caps.supported));
      },
    );
    return () => {
      active = false;
    };
  }, [initialized]);

  useEffect(() => {
    if (!initialized || mode !== "login" || !passkeysAvailable) return;
    abortConditionalRef.current?.();
    abortConditionalRef.current = startConditionalPasskeyLogin(
      (u) => onAuthenticated(u),
      (msg) => setError(msg),
    );
    return () => {
      abortConditionalRef.current?.();
      abortConditionalRef.current = null;
    };
  }, [initialized, mode, passkeysAvailable, onAuthenticated]);

  async function handlePasskeySignIn() {
    setError(null);
    abortConditionalRef.current?.();
    abortConditionalRef.current = null;
    setPasskeyBusy(true);
    try {
      const u = await triggerButtonPasskeyLogin();
      if (u) {
        onAuthenticated(u);
        return;
      }
      // Re-arm conditional autofill if user cancelled the explicit prompt
      abortConditionalRef.current = startConditionalPasskeyLogin(
        (user) => onAuthenticated(user),
        (msg) => setError(msg),
      );
    } catch (err) {
      setError(err instanceof Error ? err.message : "Passkey sign-in failed");
    } finally {
      setPasskeyBusy(false);
    }
  }

  const showEmailField = !initialized || identityMode !== "username_only";
  const showUsernameField = !initialized || identityMode !== "email_only";
  const requireEmail = !initialized || identityMode === "email_only" || identityMode === "both";
  const requireUsername = initialized && (identityMode === "username_only" || identityMode === "both");

  async function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      let res: { user: User };
      if (!initialized) {
        res = await api.setupAdmin({
          email,
          username: username.trim() || undefined,
          display_name: displayName,
          password,
        });
      } else if (mode === "invite") {
        res = await api.registerWithInvite({
          invite_code: inviteCode,
          email: email.trim() || undefined,
          username: username.trim() || undefined,
          display_name: displayName,
          password,
        });
        // Strip ?invite= from URL after successful signup
        try {
          const url = new URL(window.location.href);
          if (url.searchParams.has("invite")) {
            url.searchParams.delete("invite");
            window.history.replaceState({}, "", url.toString());
          }
        } catch {}
      } else {
        res = await api.login({ identifier, password });
      }
      onAuthenticated(res.user);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Authentication failed");
    } finally {
      setBusy(false);
    }
  }

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

        <h1 id="signin-heading" className="page-title" style={{ marginTop: 16 }}>
          {!initialized
            ? "Initialize Ourchive Hub"
            : mode === "invite"
              ? "Join with an invite key"
              : "Sign in to your archive"}
        </h1>
        <p className="page-sub" style={{ marginBottom: 24 }}>
          {!initialized
            ? "Create the first Host Admin account for this Ourchive instance."
            : mode === "invite"
              ? "Registration is invite-only. Enter your key to create an account."
              : "Use your email or username and password to access shared nodes."}
        </p>

        <form className="form-stack" onSubmit={handleSubmit}>
          {initialized && mode === "invite" && (
            <div className="field">
              <label htmlFor="auth-invite">Invite key</label>
              <span id="auth-invite-hint" className="field-hint">
                Format: OUR-XXXX-XXXX-XXXX
              </span>
              <input
                id="auth-invite"
                name="invite_code"
                type="text"
                className="mono"
                autoComplete="off"
                spellCheck={false}
                required
                aria-describedby="auth-invite-hint"
                value={inviteCode}
                onChange={(e) => setInviteCode(e.target.value)}
                placeholder="OUR-XXXX-XXXX-XXXX"
              />
            </div>
          )}

          {(!initialized || mode === "invite") && (
            <div className="field">
              <label htmlFor="auth-name">Display name</label>
              <span id="auth-name-hint" className="field-hint">
                Shown beside machines and folders you share.
              </span>
              <input
                id="auth-name"
                name="name"
                type="text"
                autoComplete="name"
                enterKeyHint="next"
                required
                aria-describedby="auth-name-hint"
                value={displayName}
                onChange={(e) => setDisplayName(e.target.value)}
                placeholder="e.g. Ru"
              />
            </div>
          )}

          {initialized && mode === "login" ? (
            <div className="field">
              <label htmlFor="auth-ident">Email or username</label>
              <input
                id="auth-ident"
                name="username"
                type="text"
                autoComplete="username webauthn"
                autoFocus
                aria-describedby="signin-heading"
                spellCheck={false}
                enterKeyHint="next"
                required
                value={identifier}
                onChange={(e) => setIdentifier(e.target.value)}
                placeholder="you@example.com or @username"
              />
            </div>
          ) : (
            <>
              {showUsernameField && (
                <div className="field">
                  <label htmlFor="auth-username">
                    Username {!requireUsername && <span className="node-sub">(optional)</span>}
                  </label>
                  <span id="auth-username-hint" className="field-hint">
                    3–32 characters: letters, numbers, ., _, -
                  </span>
                  <input
                    id="auth-username"
                    name="username"
                    type="text"
                    autoComplete="username"
                    spellCheck={false}
                    enterKeyHint="next"
                    required={requireUsername}
                    aria-describedby="auth-username-hint"
                    value={username}
                    onChange={(e) => setUsername(e.target.value)}
                    placeholder="e.g. ru"
                  />
                </div>
              )}

              {showEmailField && (
                <div className="field">
                  <label htmlFor="auth-email">
                    Email address {!requireEmail && <span className="node-sub">(optional, for recovery)</span>}
                  </label>
                  <span id="auth-email-hint" className="field-hint">
                    Format: you@example.com
                  </span>
                  <input
                    id="auth-email"
                    name="email"
                    type="email"
                    autoComplete="email"
                    spellCheck={false}
                    enterKeyHint="next"
                    required={requireEmail}
                    aria-describedby="auth-email-hint"
                    value={email}
                    onChange={(e) => setEmail(e.target.value)}
                    placeholder="you@example.com"
                  />
                </div>
              )}
            </>
          )}

          <div className="field">
            <label htmlFor="auth-password">Password</label>
            {(!initialized || mode === "invite") && (
              <span id="auth-password-hint" className="field-hint">
                At least 8 characters.
              </span>
            )}
            <div className="password-wrap">
              <input
                id="auth-password"
                name="password"
                type={showPassword ? "text" : "password"}
                autoComplete={initialized && mode === "login" ? "current-password" : "new-password"}
                enterKeyHint="done"
                required
                minLength={8}
                aria-describedby={!initialized || mode === "invite" ? "auth-password-hint" : undefined}
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
              <button
                type="button"
                className="password-toggle"
                aria-label={showPassword ? "Hide password" : "Show password"}
                onClick={() => setShowPassword((v) => !v)}
              >
                {showPassword ? <EyeOff size={15} strokeWidth={1.6} /> : <Eye size={15} strokeWidth={1.6} />}
              </button>
            </div>
          </div>

          {error && (
            <div className="alert-error" role="alert">
              {error}
            </div>
          )}

          <button type="submit" className="btn btn-primary" disabled={busy} style={{ height: 38, marginTop: 4 }}>
            {busy
              ? "Working…"
              : !initialized
                ? "Create host admin"
                : mode === "invite"
                  ? "Create account"
                  : "Sign in"}
          </button>

          {initialized && mode === "login" && passkeysAvailable && (
            <button
              type="button"
              className="btn"
              style={{ height: 36, justifyContent: "center" }}
              disabled={passkeyBusy || busy}
              onClick={() => void handlePasskeySignIn()}
            >
              <Fingerprint size={14} strokeWidth={1.6} />
              <span>{passkeyBusy ? "Waiting for passkey…" : "Sign in with passkey"}</span>
            </button>
          )}

          {initialized && oidcProviders.length > 0 && (
            <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
              {oidcProviders.map((prov) => {
                const href = `/api/auth/oidc/${encodeURIComponent(prov.id)}/login${
                  mode === "invite" && inviteCode.trim() ? `?invite=${encodeURIComponent(inviteCode.trim())}` : ""
                }`;
                return (
                  <a
                    key={prov.id}
                    href={href}
                    className="btn"
                    style={{ height: 36, justifyContent: "center", textDecoration: "none" }}
                  >
                    <Globe size={14} strokeWidth={1.6} />
                    <span>Continue with {prov.name}</span>
                  </a>
                );
              })}
            </div>
          )}

          {initialized && (
            <button
              type="button"
              className="btn"
              style={{ height: 36, justifyContent: "center" }}
              onClick={() => {
                setError(null);
                setMode((m) => (m === "login" ? "invite" : "login"));
              }}
            >
              <KeyRound size={14} strokeWidth={1.6} />
              <span>{mode === "login" ? "Have an invite key? Create account" : "Back to sign in"}</span>
            </button>
          )}
        </form>
      </div>
    </div>
  );
}

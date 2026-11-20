/**
 * CreateShareModal — Dialog for minting isolated share links (`s.` subdomain) for files or
 * folders-as-ZIP with custom URL slugs (collision-checked), URL query parameters, optional
 * Argon2id password protection, view caps, and expiration.
 */
import { FormEvent, useEffect, useMemo, useState } from "react";
import { Check, Copy, ExternalLink } from "lucide-react";
import { api, ShareLink } from "../api";
import { ModalDialog } from "./ModalDialog";

export interface ShareTarget {
  nodeId: string;
  nodeName: string;
  mountId: string;
  relPath: string;
  isDir: boolean;
  title: string;
  mimeType: string;
  sizeBytes: number;
}

interface CreateShareModalProps {
  target: ShareTarget | null;
  onCreated: (link: ShareLink) => void;
  onClose: () => void;
}

export function CreateShareModal({ target, onCreated, onClose }: CreateShareModalProps) {
  const [title, setTitle] = useState("");
  const [customSlug, setCustomSlug] = useState("");
  const [slugCheck, setSlugCheck] = useState<{ state: "idle" | "checking" | "ok" | "taken"; msg?: string }>({
    state: "idle",
  });
  const [directDownload, setDirectDownload] = useState(false);
  const [customParams, setCustomParams] = useState("");
  const [expiresHours, setExpiresHours] = useState("0");
  const [maxViews, setMaxViews] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [created, setCreated] = useState<ShareLink | null>(null);
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    if (target) {
      setTitle(target.title);
      setCustomSlug("");
      setSlugCheck({ state: "idle" });
      setDirectDownload(false);
      setCustomParams("");
      setExpiresHours("0");
      setMaxViews("");
      setPassword("");
      setError(null);
      setCreated(null);
      setCopied(false);
    }
  }, [target]);

  // Debounced live availability check for custom URL slugs
  useEffect(() => {
    const trimmed = customSlug.trim();
    if (!trimmed) {
      setSlugCheck({ state: "idle" });
      return;
    }
    setSlugCheck({ state: "checking" });
    const timer = window.setTimeout(() => {
      api
        .checkShareSlug(trimmed)
        .then((res) => {
          if (res.available) {
            setSlugCheck({ state: "ok", msg: "Available" });
          } else {
            setSlugCheck({ state: "taken", msg: res.reason || "Unavailable" });
          }
        })
        .catch(() => setSlugCheck({ state: "idle" }));
    }, 280);
    return () => window.clearTimeout(timer);
  }, [customSlug]);

  const finalURL = useMemo(() => {
    if (!created) return "";
    try {
      const u = new URL(created.url);
      if (directDownload) {
        u.pathname = u.pathname.replace(/\/+$/, "") + "/raw";
        u.searchParams.set("download", "1");
      }
      if (customParams.trim()) {
        const extra = new URLSearchParams(customParams.trim().replace(/^\?/, ""));
        extra.forEach((v, k) => {
          if (k.trim()) u.searchParams.set(k.trim(), v);
        });
      }
      return u.toString();
    } catch {
      return created.url;
    }
  }, [created, directDownload, customParams]);

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    if (!target) return;
    if (customSlug.trim() && slugCheck.state === "taken") {
      setError(slugCheck.msg || "Custom URL slug is not available");
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const res = await api.createShare({
        node_id: target.nodeId,
        mount_id: target.mountId,
        rel_path: target.relPath,
        is_dir: target.isDir,
        title: title.trim() || target.title,
        mime_type: target.mimeType,
        size_bytes: target.sizeBytes,
        custom_slug: customSlug.trim() || undefined,
        password: password.trim() || undefined,
        max_views: maxViews ? Math.max(1, Math.round(Number(maxViews))) : 0,
        expires_in_hours: Number(expiresHours) || 0,
      });
      setCreated(res.share);
      onCreated(res.share);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not create share link");
    } finally {
      setBusy(false);
    }
  }

  async function copyURL(url: string) {
    try {
      await navigator.clipboard.writeText(url);
      setCopied(true);
      setTimeout(() => setCopied(false), 1800);
    } catch {}
  }

  return (
    <ModalDialog
      open={Boolean(target)}
      title={target?.isDir ? "Share folder as ZIP" : "Create share link"}
      onClose={onClose}
    >
      {target && !created && (
        <form className="form-stack" onSubmit={handleSubmit}>
          <p className="page-sub" style={{ marginTop: 0 }}>
            {target.isDir
              ? `Streams "${target.title}" on-the-fly as a ZIP archive directly from ${target.nodeName}.`
              : `Generates an isolated embed link on your share subdomain hosted by ${target.nodeName}.`}
          </p>

          <div className="field">
            <label htmlFor="share-title">Title</label>
            <input
              id="share-title"
              type="text"
              required
              value={title}
              onChange={(e) => setTitle(e.target.value)}
            />
          </div>

          <div className="field">
            <label htmlFor="share-slug">
              Custom URL slug <span className="inline-muted">(optional)</span>
            </label>
            <span className="field-hint">
              Leave blank for a random 8-char link. Existing links are never overwritten.
            </span>
            <input
              id="share-slug"
              type="text"
              className="mono"
              placeholder="e.g. demo-clip-2026"
              value={customSlug}
              onChange={(e) => setCustomSlug(e.target.value)}
            />
            {slugCheck.state === "checking" && <span className="field-hint">Checking availability…</span>}
            {slugCheck.state === "ok" && <span className="slug-status-ok">✓ {slugCheck.msg}</span>}
            {slugCheck.state === "taken" && <span className="slug-status-err">✕ {slugCheck.msg}</span>}
          </div>

          <div className="settings-grid-2">
            <div className="field">
              <label htmlFor="share-expiry">Link lifetime</label>
              <select id="share-expiry" value={expiresHours} onChange={(e) => setExpiresHours(e.target.value)}>
                <option value="0">Permanent (until revoked)</option>
                <option value="1">Temporary · 1 hour</option>
                <option value="24">Temporary · 24 hours</option>
                <option value="168">Temporary · 7 days</option>
                <option value="720">Temporary · 30 days</option>
              </select>
            </div>

            <div className="field">
              <label htmlFor="share-views">Max views (optional)</label>
              <input
                id="share-views"
                type="number"
                min={1}
                inputMode="numeric"
                placeholder="Unlimited"
                value={maxViews}
                onChange={(e) => setMaxViews(e.target.value)}
              />
            </div>
          </div>

          <div className="field">
            <label htmlFor="share-params">
              URL query parameters <span className="inline-muted">(optional)</span>
            </label>
            <input
              id="share-params"
              type="text"
              className="mono"
              placeholder="e.g. t=15&ref=discord"
              value={customParams}
              onChange={(e) => setCustomParams(e.target.value)}
            />
          </div>

          {!target.isDir && (
            <label className="switch-row">
              <span>
                <strong>Direct download link</strong>
                <small>Points straight to /raw?download=1 instead of the preview page</small>
              </span>
              <input
                type="checkbox"
                checked={directDownload}
                onChange={(e) => setDirectDownload(e.target.checked)}
                {...({ switch: "" } as Record<string, string>)}
              />
            </label>
          )}

          <div className="field">
            <label htmlFor="share-password">Password (optional)</label>
            <span className="field-hint">Password-protected links hide media previews from bots until unlocked.</span>
            <input
              id="share-password"
              type="password"
              autoComplete="new-password"
              placeholder="Leave blank for public link"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          </div>

          {error && (
            <div className="alert-error" role="alert">
              {error}
            </div>
          )}

          <div className="modal-actions">
            <button type="button" className="btn" onClick={onClose}>
              Cancel
            </button>
            <button
              type="submit"
              className="btn btn-primary"
              disabled={busy || slugCheck.state === "taken" || slugCheck.state === "checking"}
            >
              {busy ? "Creating…" : "Create share link"}
            </button>
          </div>
        </form>
      )}

      {created && (
        <div className="form-stack">
          <p className="page-sub" style={{ marginTop: 0 }}>
            Your share link is live. Paste it in Discord, Slack, or a browser for rich previews and direct streaming.
          </p>

          <div className="code-block">
            <div className="code-head">
              <span>{created.is_dir ? "Folder ZIP link" : "Share link"}</span>
              <div style={{ display: "inline-flex", gap: 6 }}>
                <button type="button" className="btn btn-sm" onClick={() => copyURL(finalURL)}>
                  {copied ? <Check size={13} /> : <Copy size={13} />}
                  <span>{copied ? "Copied" : "Copy"}</span>
                </button>
                <a className="btn btn-sm" href={finalURL} target="_blank" rel="noopener noreferrer">
                  <ExternalLink size={13} />
                  <span>Open</span>
                </a>
              </div>
            </div>
            <pre className="mono">{finalURL}</pre>
          </div>

          <div className="modal-actions">
            <button type="button" className="btn btn-accent" onClick={onClose}>
              Done
            </button>
          </div>
        </div>
      )}
    </ModalDialog>
  );
}

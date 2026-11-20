/**
 * CreateShareModal — Dialog for minting isolated share links (`s.` subdomain) for files or
 * folders-as-ZIP with optional Argon2id password protection, view caps, and expiration.
 */
import { FormEvent, useEffect, useState } from "react";
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
      setExpiresHours("0");
      setMaxViews("");
      setPassword("");
      setError(null);
      setCreated(null);
      setCopied(false);
    }
  }, [target]);

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    if (!target) return;
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
            <button type="submit" className="btn btn-primary" disabled={busy}>
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
                <button type="button" className="btn btn-sm" onClick={() => copyURL(created.url)}>
                  {copied ? <Check size={13} /> : <Copy size={13} />}
                  <span>{copied ? "Copied" : "Copy"}</span>
                </button>
                <a className="btn btn-sm" href={created.url} target="_blank" rel="noopener noreferrer">
                  <ExternalLink size={13} />
                  <span>Open</span>
                </a>
              </div>
            </div>
            <pre className="mono">{created.url}</pre>
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

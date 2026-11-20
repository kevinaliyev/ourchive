/**
 * SharesView — Active share link directory showing file/folder targets, view counts,
 * password/expiry badges, online host status, one-click link copy, and revocation.
 */
import { useState } from "react";
import { Archive, Check, Copy, ExternalLink, File as FileIcon, Link2, Lock, Trash2 } from "lucide-react";
import { api, formatBytes, formatDate, ShareLink } from "../api";

interface SharesViewProps {
  shares: ShareLink[];
  onRefresh: () => Promise<void>;
  onGoToExplorer: () => void;
}

export function SharesView({ shares, onRefresh, onGoToExplorer }: SharesViewProps) {
  const [copiedSlug, setCopiedSlug] = useState<string | null>(null);

  async function copyLink(link: ShareLink) {
    try {
      await navigator.clipboard.writeText(link.url);
      setCopiedSlug(link.slug);
      setTimeout(() => setCopiedSlug(null), 1800);
    } catch {}
  }

  async function handleRevoke(link: ShareLink) {
    if (!window.confirm(`Revoke share link "${link.title}"? Anyone with this URL will lose access immediately.`)) {
      return;
    }
    await api.deleteShare(link.slug);
    await onRefresh();
  }

  if (shares.length === 0) {
    return (
      <section className="card empty-card">
        <Link2 size={24} strokeWidth={1.4} className="muted-icon" />
        <div className="empty-title">No share links yet</div>
        <p className="empty-copy">
          Select any file or folder in the Explorer to create a permanent or temporary share link with rich social
          embeds and zero-buffered streaming.
        </p>
        <button type="button" className="btn btn-primary" onClick={onGoToExplorer}>
          Browse archive
        </button>
      </section>
    );
  }

  return (
    <section className="card">
      <div className="section-head">
        <div>
          <h2 className="section-title">Active &amp; recent share links</h2>
          <p className="page-sub">
            Served on your isolated share subdomain with OpenGraph embeds, Range streaming, and folder-as-ZIP support.
          </p>
        </div>
      </div>

      <div tabIndex={0} role="region" aria-label="Share links" className="vfs-table-wrapper">
        <table className="vfs-table">
          <thead>
            <tr>
              <th>Item</th>
              <th>Host</th>
              <th>Views</th>
              <th>Lifetime</th>
              <th className="num-col">Actions</th>
            </tr>
          </thead>
          <tbody>
            {shares.map((s) => {
              const isOffline = s.node_status === "offline";
              return (
                <tr key={s.slug} className={isOffline || s.expired ? "offline-row" : undefined}>
                  <th>
                    <div className="file-cell">
                      {s.is_dir ? (
                        <Archive size={16} strokeWidth={1.5} className="icon-folder" />
                      ) : (
                        <FileIcon size={16} strokeWidth={1.5} />
                      )}
                      <div>
                        <div className="node-title-row">
                          <span className="file-name">{s.title}</span>
                          {s.is_dir && <span className="tag-chip mono">ZIP</span>}
                          {s.has_password && (
                            <span className="tag-chip" title="Password protected">
                              <Lock size={11} />
                            </span>
                          )}
                          {s.expired && <span className="reject-badge">Expired</span>}
                        </div>
                        <div className="mount-path mono">
                          {s.url}
                          {!s.is_dir && s.size_bytes > 0 && ` · ${formatBytes(s.size_bytes)}`}
                        </div>
                      </div>
                    </div>
                  </th>
                  <td>
                    <span className="node-title-row">
                      <span className={`dot ${isOffline ? "offline" : s.node_status === "paused" ? "paused" : ""}`} />
                      <span>{s.node_name}</span>
                    </span>
                  </td>
                  <td className="mono">
                    {s.view_count}
                    {s.max_views ? ` / ${s.max_views}` : ""}
                  </td>
                  <td className="mono">{s.expires_at ? `Until ${formatDate(s.expires_at)}` : "Permanent"}</td>
                  <td className="num-col">
                    <div className="row-actions">
                      <button type="button" className="btn btn-sm" onClick={() => copyLink(s)}>
                        {copiedSlug === s.slug ? <Check size={13} /> : <Copy size={13} />}
                        <span>{copiedSlug === s.slug ? "Copied" : "Copy link"}</span>
                      </button>
                      <a
                        className="icon-btn"
                        href={s.url}
                        target="_blank"
                        rel="noopener noreferrer"
                        title="Open share page"
                        aria-label={`Open ${s.title}`}
                      >
                        <ExternalLink size={14} strokeWidth={1.6} />
                      </a>
                      <button
                        type="button"
                        className="icon-btn danger"
                        title="Revoke link"
                        aria-label={`Revoke ${s.title}`}
                        onClick={() => handleRevoke(s)}
                      >
                        <Trash2 size={14} strokeWidth={1.5} />
                      </button>
                    </div>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </section>
  );
}

/**
 * SharesView — Active share link directory with whole-screen drag-and-drop and
 * primary file picker. Dropping or choosing a file checks the archive first
 * (name + exact byte size) to reuse an existing copy without cloning; otherwise
 * it uploads a loose file under `Shared/<username>/` on the user's own online
 * mount via egress-safe P2P and opens CreateShareModal.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Archive,
  Check,
  Copy,
  ExternalLink,
  File as FileIcon,
  FolderOpen,
  Link2,
  Lock,
  Trash2,
  Upload,
} from "lucide-react";
import {
  api,
  formatBytes,
  formatDate,
  NodeView,
  PublicNetworkConfig,
  ShareLink,
  User,
} from "../api";
import { uploadFileSmart } from "../p2p";
import { CreateShareModal, ShareTarget } from "./CreateShareModal";

interface SharesViewProps {
  currentUser: User;
  nodes: NodeView[];
  netConfig: PublicNetworkConfig | null;
  shares: ShareLink[];
  onRefresh: () => Promise<void>;
  onGoToExplorer: () => void;
}

interface WritableTarget {
  key: string;
  nodeId: string;
  nodeName: string;
  mountId: string;
  mountName: string;
}

function sanitizeUserFolder(username: string): string {
  const cleaned = username.trim().replace(/[^a-zA-Z0-9._-]+/g, "_");
  return cleaned || "user";
}

export function SharesView({
  currentUser,
  nodes,
  netConfig,
  shares,
  onRefresh,
  onGoToExplorer,
}: SharesViewProps) {
  const [copiedSlug, setCopiedSlug] = useState<string | null>(null);
  const [dragging, setDragging] = useState(false);
  const [busyStatus, setBusyStatus] = useState<string | null>(null);
  const [uploadPct, setUploadPct] = useState<number | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [shareTarget, setShareTarget] = useState<ShareTarget | null>(null);
  const [selectedDestKey, setSelectedDestKey] = useState<string>("");
  const fileInputRef = useRef<HTMLInputElement | null>(null);
  const dragDepthRef = useRef(0);

  // User's own online folder mounts eligible to store new loose shared files
  const ownWritableMounts = useMemo<WritableTarget[]>(() => {
    const out: WritableTarget[] = [];
    for (const n of nodes) {
      if (!n.is_owner || n.status !== "online") continue;
      for (const m of n.mounts) {
        if (!m.enabled || m.rejected || m.kind === "file") continue;
        out.push({
          key: `${n.id}::${m.id}`,
          nodeId: n.id,
          nodeName: n.display_name,
          mountId: m.id,
          mountName: m.name,
        });
      }
    }
    return out;
  }, [nodes]);

  const activeDest = useMemo(() => {
    if (ownWritableMounts.length === 0) return null;
    return ownWritableMounts.find((d) => d.key === selectedDestKey) ?? ownWritableMounts[0];
  }, [ownWritableMounts, selectedDestKey]);

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

  /**
   * Shared handler for both whole-screen drag-and-drop and file-picker selection:
   * 1. Search the archive for an existing file with the same name and exact byte size.
   *    If found, reuse it directly without cloning/re-uploading.
   * 2. Otherwise, upload as a loose file under `Shared/<username>/<filename>` on the
   *    user's own online mount via egress-safe P2P, then open CreateShareModal.
   */
  const handleFileForShare = useCallback(
    async (file: File) => {
      setError(null);
      setNotice(null);
      setUploadPct(null);
      setBusyStatus(`Checking archive for "${file.name}"…`);

      try {
        // Step 1: Duplicate check across all online archive nodes
        try {
          const search = await api.searchFiles(file.name);
          const exactMatches = (search.hits ?? []).filter(
            (h) => !h.is_dir && h.name.toLowerCase() === file.name.toLowerCase() && h.size === file.size,
          );
          const match = exactMatches.find((h) => h.is_own_host) ?? exactMatches[0];
          if (match) {
            setBusyStatus(null);
            setNotice(
              `Found existing copy in ${match.node_name} / ${match.mount_name}/${match.path} — sharing without re-uploading.`,
            );
            setShareTarget({
              nodeId: match.node_id,
              nodeName: match.node_name,
              mountId: match.mount_id,
              relPath: match.path,
              isDir: false,
              title: match.name,
              mimeType: match.mime_type || file.type || "application/octet-stream",
              sizeBytes: match.size,
            });
            return;
          }
        } catch {
          // Non-fatal: proceed to upload into the user's own mount
        }

        // Step 2: Upload as a loose file under Shared/<username>/<filename>
        if (!activeDest) {
          setBusyStatus(null);
          setError(
            "No online shared folder on your own machine to store a new loose file. Connect a machine and share a folder in Contribution settings first.",
          );
          return;
        }

        const userDir = `Shared/${sanitizeUserFolder(
          currentUser.username || currentUser.display_name || currentUser.email || "user",
        )}`;
        setBusyStatus(`Preparing ${activeDest.mountName}/${userDir}…`);
        try {
          await api.mkdir(activeDest.nodeId, activeDest.mountId, "Shared");
        } catch {}
        try {
          await api.mkdir(activeDest.nodeId, activeDest.mountId, userDir);
        } catch {}

        const relPath = `${userDir}/${file.name}`;
        setBusyStatus(`Uploading "${file.name}" via P2P…`);
        setUploadPct(0);

        const res = await uploadFileSmart(
          activeDest.nodeId,
          activeDest.mountId,
          relPath,
          file,
          netConfig,
          (p) => {
            const pct = p.total > 0 ? Math.min(100, Math.round((p.received / p.total) * 100)) : 0;
            setUploadPct(pct);
            setBusyStatus(
              `Uploading "${file.name}" (${p.transport === "p2p" ? "P2P" : "Relay"} · ${pct}%)…`,
            );
          },
        );

        setBusyStatus(null);
        setUploadPct(null);
        setNotice(
          `Saved loose file to ${activeDest.nodeName} / ${activeDest.mountName}/${res.path} (${res.transport === "p2p" ? "Direct P2P" : "Hub Relay"}).`,
        );
        setShareTarget({
          nodeId: activeDest.nodeId,
          nodeName: activeDest.nodeName,
          mountId: activeDest.mountId,
          relPath: res.path || relPath,
          isDir: false,
          title: file.name,
          mimeType: file.type || "application/octet-stream",
          sizeBytes: file.size,
        });
      } catch (err) {
        setBusyStatus(null);
        setUploadPct(null);
        setError(err instanceof Error ? err.message : "Failed to prepare file for sharing");
      }
    },
    [activeDest, currentUser, netConfig],
  );

  // Whole-screen drag-and-drop listeners while on the Share links tab
  useEffect(() => {
    const hasFiles = (e: DragEvent) => Array.from(e.dataTransfer?.types ?? []).includes("Files");

    const onDragEnter = (e: DragEvent) => {
      if (!hasFiles(e)) return;
      e.preventDefault();
      dragDepthRef.current += 1;
      if (!busyStatus) setDragging(true);
    };
    const onDragOver = (e: DragEvent) => {
      if (!hasFiles(e)) return;
      e.preventDefault();
      if (e.dataTransfer) e.dataTransfer.dropEffect = "copy";
    };
    const onDragLeave = (e: DragEvent) => {
      if (!hasFiles(e)) return;
      dragDepthRef.current = Math.max(0, dragDepthRef.current - 1);
      if (dragDepthRef.current === 0) setDragging(false);
    };
    const onDrop = (e: DragEvent) => {
      if (!hasFiles(e)) return;
      e.preventDefault();
      dragDepthRef.current = 0;
      setDragging(false);
      if (busyStatus) return;
      const file = e.dataTransfer?.files?.[0];
      if (file) void handleFileForShare(file);
    };

    window.addEventListener("dragenter", onDragEnter);
    window.addEventListener("dragover", onDragOver);
    window.addEventListener("dragleave", onDragLeave);
    window.addEventListener("drop", onDrop);
    return () => {
      window.removeEventListener("dragenter", onDragEnter);
      window.removeEventListener("dragover", onDragOver);
      window.removeEventListener("dragleave", onDragLeave);
      window.removeEventListener("drop", onDrop);
    };
  }, [busyStatus, handleFileForShare]);

  const statusBanners = (
    <>
      {busyStatus && (
        <div className="status-banner" role="status">
          <span className="mono">{busyStatus}</span>
          {uploadPct !== null && (
            <div className="quick-share-bar" aria-hidden="true">
              <div className="quick-share-bar-fill" style={{ width: `${uploadPct}%` }} />
            </div>
          )}
        </div>
      )}
      {notice && !busyStatus && (
        <div className="status-banner" role="status">
          <span>{notice}</span>
        </div>
      )}
      {error && (
        <div className="alert-error" role="alert">
          {error}
        </div>
      )}
    </>
  );

  return (
    <div className="section-stack">
      <input
        ref={fileInputRef}
        type="file"
        hidden
        onChange={(e) => {
          const file = e.target.files?.[0];
          e.target.value = "";
          if (file) void handleFileForShare(file);
        }}
      />

      {dragging && (
        <div className="screen-drop-overlay" aria-live="polite">
          <div className="screen-drop-card">
            <Upload size={26} strokeWidth={1.5} />
            <div className="screen-drop-title">Drop file to create share link</div>
            <p className="page-sub" style={{ margin: 0 }}>
              Existing archive files are linked automatically without cloning.
            </p>
          </div>
        </div>
      )}

      {shares.length === 0 ? (
        <section className="card empty-card">
          <Link2 size={24} strokeWidth={1.4} className="muted-icon" />
          <div className="empty-title">No share links yet</div>
          <p className="empty-copy">
            Drop any file anywhere on this screen or choose one below to mint a share link. Existing files in your
            archive are reused automatically without cloning.
          </p>

          {statusBanners}

          <div className="share-empty-actions">
            {ownWritableMounts.length > 1 && activeDest && (
              <select
                className="sort-select"
                aria-label="Destination folder for new loose shared files"
                value={activeDest.key}
                onChange={(e) => setSelectedDestKey(e.target.value)}
                disabled={Boolean(busyStatus)}
              >
                {ownWritableMounts.map((d) => (
                  <option key={d.key} value={d.key}>
                    {d.nodeName} / {d.mountName}
                  </option>
                ))}
              </select>
            )}
            <button
              type="button"
              className="btn btn-primary"
              disabled={Boolean(busyStatus)}
              onClick={() => fileInputRef.current?.click()}
            >
              <Upload size={15} strokeWidth={1.6} />
              <span>Choose file to share</span>
            </button>
          </div>

          <button type="button" className="btn btn-sm share-browse-sub" onClick={onGoToExplorer}>
            <FolderOpen size={13} strokeWidth={1.5} />
            <span>or browse archive for an existing file</span>
          </button>
        </section>
      ) : (
        <section className="card">
          <div className="section-head">
            <div>
              <h2 className="section-title">Active &amp; recent share links</h2>
              <p className="page-sub">
                Served on your isolated share subdomain with OpenGraph embeds, Range streaming, and folder-as-ZIP
                support. Drop a file anywhere to create a new link.
              </p>
            </div>
            <div className="quick-share-actions">
              {ownWritableMounts.length > 1 && activeDest && (
                <select
                  className="sort-select"
                  aria-label="Destination folder for new loose shared files"
                  value={activeDest.key}
                  onChange={(e) => setSelectedDestKey(e.target.value)}
                  disabled={Boolean(busyStatus)}
                >
                  {ownWritableMounts.map((d) => (
                    <option key={d.key} value={d.key}>
                      {d.nodeName} / {d.mountName}
                    </option>
                  ))}
                </select>
              )}
              <button
                type="button"
                className="btn btn-primary"
                disabled={Boolean(busyStatus)}
                onClick={() => fileInputRef.current?.click()}
              >
                <Upload size={15} strokeWidth={1.6} />
                <span>Share a file</span>
              </button>
            </div>
          </div>

          {statusBanners}

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
                          <span
                            className={`dot ${isOffline ? "offline" : s.node_status === "paused" ? "paused" : ""}`}
                          />
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

          <div className="share-table-footer">
            <button type="button" className="btn btn-sm share-browse-sub" onClick={onGoToExplorer}>
              <FolderOpen size={13} strokeWidth={1.5} />
              <span>Browse archive to share an existing file or folder</span>
            </button>
          </div>
        </section>
      )}

      <CreateShareModal
        target={shareTarget}
        onCreated={() => {
          void onRefresh();
        }}
        onClose={() => setShareTarget(null)}
      />
    </div>
  );
}

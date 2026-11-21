/**
 * ContributionView — Machine enrollment, shared folder/file mount management, upload caps,
 * opt-in local mirroring consent, GPU compute participation, and vendor-colored hardware readout.
 */
import { FormEvent, useState } from "react";
import { Check, Copy, Cpu, FilePlus2, FolderOpen, FolderPlus, Microchip, Monitor, Plus, Trash2 } from "lucide-react";
import { api, EnrollTokenResponse, formatMbps, NodeView } from "../api";
import { ModalDialog } from "./ModalDialog";

function vendorClass(name?: string): "hw-amd" | "hw-intel" | "hw-nvidia" | "hw-unknown" {
  const s = (name ?? "").toLowerCase();
  if (/(amd|ryzen|radeon|threadripper|epyc|amf)/.test(s)) return "hw-amd";
  if (/(nvidia|geforce|rtx|gtx|quadro|tesla|nvenc)/.test(s)) return "hw-nvidia";
  if (/(intel|core|xeon|arc|uhd|iris|qsv)/.test(s)) return "hw-intel";
  return "hw-unknown";
}

interface ContributionViewProps {
  nodes: NodeView[];
  onRefresh: () => Promise<void>;
}

export function ContributionView({ nodes, onRefresh }: ContributionViewProps) {
  const [enrollOpen, setEnrollOpen] = useState(false);
  const [machineName, setMachineName] = useState("");
  const [enrollResult, setEnrollResult] = useState<EnrollTokenResponse | null>(null);
  const [enrollBusy, setEnrollBusy] = useState(false);
  const [installTab, setInstallTab] = useState<"windows" | "unix" | "source">(() =>
    typeof navigator !== "undefined" && /windows/i.test(navigator.userAgent) ? "windows" : "unix"
  );
  const [copiedKey, setCopiedKey] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const [mountDrafts, setMountDrafts] = useState<
    Record<string, { name: string; local_path: string; kind: "folder" | "file" }>
  >({});
  const [capDrafts, setCapDrafts] = useState<Record<string, string>>({});
  const [pickingNodeId, setPickingNodeId] = useState<string | null>(null);

  async function handlePick(node: NodeView, kind: "folder" | "file") {
    setError(null);
    setPickingNodeId(node.id);
    try {
      const res = kind === "file" ? await api.pickFile(node.id) : await api.pickFolder(node.id);
      if (!res.cancelled && res.path) {
        setMountDrafts((prev) => {
          const cur = prev[node.id] ?? { name: "", local_path: "", kind };
          return {
            ...prev,
            [node.id]: {
              name: cur.name.trim() || res.name || (kind === "file" ? "File" : "Shared"),
              local_path: res.path!,
              kind,
            },
          };
        });
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : `Could not open ${kind} picker on this machine`);
    } finally {
      setPickingNodeId(null);
    }
  }

  async function handleGenerateToken(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setEnrollBusy(true);
    try {
      const res = await api.createEnrollToken(machineName);
      setEnrollResult(res);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not generate enrollment token");
    } finally {
      setEnrollBusy(false);
    }
  }

  async function copyText(key: string, text: string) {
    try {
      await navigator.clipboard.writeText(text);
      setCopiedKey(key);
      setTimeout(() => setCopiedKey(null), 1800);
    } catch {}
  }

  async function handleTogglePaused(node: NodeView, paused: boolean) {
    await api.updateNodeConfig(node.id, { paused });
    await onRefresh();
  }

  async function handleToggleGPU(node: NodeView, allow_group_compute: boolean) {
    await api.updateNodeConfig(node.id, { allow_group_compute });
    await onRefresh();
  }

  async function handleSaveUploadCap(node: NodeView) {
    const raw = capDrafts[node.id] ?? String(node.upload_cap_mbps);
    const parsed = Math.max(0, Math.round(Number(raw) || 0));
    await api.updateNodeConfig(node.id, { upload_cap_mbps: parsed });
    await onRefresh();
  }

  async function handleAddMount(e: FormEvent, nodeId: string) {
    e.preventDefault();
    const draft = mountDrafts[nodeId];
    if (!draft?.name.trim() || !draft?.local_path.trim()) return;
    setError(null);
    try {
      await api.addMount(nodeId, {
        name: draft.name.trim(),
        local_path: draft.local_path.trim(),
        kind: draft.kind,
      });
      setMountDrafts((prev) => ({ ...prev, [nodeId]: { name: "", local_path: "", kind: "folder" } }));
      await onRefresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to add shared path");
    }
  }

  async function handleToggleMount(nodeId: string, mountId: string, enabled: boolean) {
    await api.updateMount(nodeId, mountId, { enabled });
    await onRefresh();
  }

  async function handleToggleMountSync(nodeId: string, mountId: string, allow_sync: boolean) {
    await api.updateMount(nodeId, mountId, { allow_sync });
    await onRefresh();
  }

  async function handleDeleteMount(nodeId: string, mountId: string) {
    await api.deleteMount(nodeId, mountId);
    await onRefresh();
  }

  async function handleDeleteNode(node: NodeView) {
    if (!window.confirm(`Remove machine "${node.display_name}"? Its headless agent will be disconnected.`)) return;
    await api.deleteNode(node.id);
    await onRefresh();
  }

  return (
    <div className="section-stack">
      <div className="toolbar-row">
        <div className="toolbar-meta">
          <span>
            {nodes.length} {nodes.length === 1 ? "machine" : "machines"} registered
          </span>
          <span className="sep">·</span>
          <span>{nodes.filter((n) => n.status === "online").length} connected</span>
        </div>

        <button
          type="button"
          className="btn btn-primary"
          onClick={() => {
            setEnrollResult(null);
            setMachineName("");
            setEnrollOpen(true);
          }}
        >
          <Plus size={15} strokeWidth={1.6} />
          <span>Connect a machine</span>
        </button>
      </div>

      {error && (
        <div className="alert-error" role="alert">
          {error}
        </div>
      )}

      {nodes.length === 0 ? (
        <section className="card empty-card">
          <Monitor size={24} strokeWidth={1.4} className="muted-icon" />
          <div className="empty-title">No contributor machines connected yet</div>
          <p className="empty-copy">
            Install the lightweight headless agent on any PC, NAS, or Mac to share selected folders and optional GPU
            conversion power — zero port forwarding required.
          </p>
          <button
            type="button"
            className="btn btn-primary"
            onClick={() => {
              setEnrollResult(null);
              setEnrollOpen(true);
            }}
          >
            <Plus size={15} strokeWidth={1.6} />
            <span>Connect your first machine</span>
          </button>
        </section>
      ) : (
        <div className="node-grid">
          {nodes.map((node) => {
            const isOffline = node.status === "offline";
            const isPaused = node.status === "paused";
            const draft = mountDrafts[node.id] ?? { name: "", local_path: "", kind: "folder" as const };
            const capValue = capDrafts[node.id] ?? String(node.upload_cap_mbps);

            return (
              <section key={node.id} className={`card node-card ${isOffline ? "offline" : ""}`}>
                <div className="node-head">
                  <div className="node-ident">
                    <div className="node-title-row">
                      <span className={`dot ${isOffline ? "offline" : isPaused ? "paused" : ""}`} />
                      <h2 className="node-name">{node.display_name}</h2>
                      <span className="tag-chip mono">
                        {node.os || "unknown"}/{node.arch || "unknown"}
                      </span>
                      <span className="tag-chip">
                        {isOffline ? "Offline" : isPaused ? "Paused" : "Connected"}
                      </span>
                    </div>
                    <div className="hw-line">
                      <span className="hw-owner">Owner: {node.owner_name}</span>
                      {(node.compute.cpu_model || node.compute.cpu_cores > 0) && (
                        <>
                          <span className="sep">·</span>
                          <span className={`hw-item ${vendorClass(node.compute.cpu_model)}`}>
                            <Cpu size={14} strokeWidth={1.6} />
                            <span>
                              {node.compute.cpu_model || `${node.compute.cpu_cores} cores`}
                              {node.compute.cpu_model && node.compute.cpu_cores > 0 ? ` (${node.compute.cpu_cores}c)` : ""}
                            </span>
                            <span className="hw-role">CPU</span>
                          </span>
                        </>
                      )}
                      {node.compute.gpu_model && (
                        <>
                          <span className="sep">·</span>
                          <span className={`hw-item ${vendorClass(node.compute.gpu_model)}`}>
                            <Microchip size={17} strokeWidth={1.6} />
                            <span>{node.compute.gpu_model}</span>
                            <span className="hw-role">GPU</span>
                          </span>
                        </>
                      )}
                      {node.compute.hw_encoders && node.compute.hw_encoders.length > 0 && (
                        <>
                          <span className="sep">·</span>
                          <span className="hw-codecs mono">
                            Codecs: {node.compute.hw_encoders.join(", ")}
                          </span>
                        </>
                      )}
                    </div>
                  </div>

                  <div className="node-head-right">
                    <div
                      className="io-pair"
                      aria-label={`Live bandwidth on ${node.display_name}`}
                      title={`Live bandwidth on ${node.display_name}${node.is_owner ? " (this PC)" : ""} — ↑ upload from this machine · ↓ download to this machine`}
                    >
                      <span className="io-up">↑ {formatMbps(node.traffic.upload_bps)}</span>
                      <span className="io-down">↓ {formatMbps(node.traffic.download_bps)}</span>
                    </div>
                    {node.is_owner && (
                      <button
                        type="button"
                        className="icon-btn danger"
                        title="Remove machine"
                        aria-label={`Remove ${node.display_name}`}
                        onClick={() => handleDeleteNode(node)}
                      >
                        <Trash2 size={15} strokeWidth={1.5} />
                      </button>
                    )}
                  </div>
                </div>

                {node.is_owner && (
                  <div className="node-controls">
                    <label className="switch-row">
                      <span>
                        <strong>Pause sharing</strong>
                        <small>Temporarily stop serving files from this machine</small>
                      </span>
                      <input
                        type="checkbox"
                        checked={node.paused}
                        onChange={(e) => handleTogglePaused(node, e.target.checked)}
                        {...({ switch: "" } as Record<string, string>)}
                      />
                    </label>

                    <label className="switch-row">
                      <span>
                        <strong>Share GPU for group conversions</strong>
                        <small>Allow members to run hardware ffmpeg jobs on this machine</small>
                      </span>
                      <input
                        type="checkbox"
                        checked={node.allow_group_compute}
                        onChange={(e) => handleToggleGPU(node, e.target.checked)}
                        {...({ switch: "" } as Record<string, string>)}
                      />
                    </label>

                    <div className="cap-row">
                      <div>
                        <strong>Upload speed limit</strong>
                        <small>0 Mbps means unlimited</small>
                      </div>
                      <div className="cap-input-group">
                        <input
                          type="number"
                          min={0}
                          max={10000}
                          inputMode="numeric"
                          className="mono cap-input"
                          aria-label={`Upload speed limit in Mbps for ${node.display_name}`}
                          value={capValue}
                          onChange={(e) => setCapDrafts((prev) => ({ ...prev, [node.id]: e.target.value }))}
                        />
                        <span className="unit-label mono">Mbps</span>
                        <button type="button" className="btn" onClick={() => handleSaveUploadCap(node)}>
                          Apply
                        </button>
                      </div>
                    </div>
                  </div>
                )}

                <div className="mounts-block">
                  <div className="mounts-head">Shared folders &amp; individual file references</div>
                  {node.mounts.length === 0 ? (
                    <div className="mounts-empty">No folders or files referenced yet.</div>
                  ) : (
                    <ul className="mount-list">
                      {node.mounts.map((m) => (
                        <li key={m.id} className={`mount-item ${m.rejected ? "rejected" : ""}`}>
                          <div className="mount-info">
                            <div className="mount-name-row">
                              <span className="mount-name">{m.name}</span>
                              <span className="tag-chip mono">{m.kind === "file" ? "File" : "Folder"}</span>
                              {m.rejected && <span className="reject-badge">{m.reject_msg || "Path unavailable"}</span>}
                            </div>
                            <div className="mount-path mono">{m.local_path}</div>
                          </div>
                          {node.is_owner && (
                            <div className="mount-actions">
                              {m.kind !== "file" && (
                                <label className="inline-switch" title="Let other members opt in to keep a local backup copy of this folder">
                                  <span>Allow mirroring</span>
                                  <input
                                    type="checkbox"
                                    checked={Boolean(m.allow_sync)}
                                    aria-label={`Allow mirroring for ${m.name}`}
                                    onChange={(e) => handleToggleMountSync(node.id, m.id, e.target.checked)}
                                    {...({ switch: "" } as Record<string, string>)}
                                  />
                                </label>
                              )}
                              <label className="inline-switch" title="Share this item in the archive">
                                <span>Shared</span>
                                <input
                                  type="checkbox"
                                  checked={m.enabled}
                                  aria-label={`Share ${m.name}`}
                                  onChange={(e) => handleToggleMount(node.id, m.id, e.target.checked)}
                                  {...({ switch: "" } as Record<string, string>)}
                                />
                              </label>
                              <button
                                type="button"
                                className="icon-btn danger"
                                aria-label={`Remove ${m.name}`}
                                onClick={() => handleDeleteMount(node.id, m.id)}
                              >
                                <Trash2 size={15} strokeWidth={1.6} />
                              </button>
                            </div>
                          )}
                        </li>
                      ))}
                    </ul>
                  )}

                  {node.is_owner && (
                    <div className="add-mount-block">
                      <form className="add-mount-form" onSubmit={(e) => handleAddMount(e, node.id)}>
                        <input
                          type="text"
                          placeholder="Display name (e.g. Clips)"
                          required
                          value={draft.name}
                          onChange={(e) =>
                            setMountDrafts((prev) => ({
                              ...prev,
                              [node.id]: { ...draft, name: e.target.value },
                            }))
                          }
                        />
                        <div className="path-input-group">
                          <input
                            type="text"
                            className="mono"
                            placeholder="Folder or single file path (~/Videos, D:\clip.mp4, …)"
                            required
                            value={draft.local_path}
                            onChange={(e) =>
                              setMountDrafts((prev) => ({
                                ...prev,
                                [node.id]: { ...draft, local_path: e.target.value },
                              }))
                            }
                          />
                          {node.status === "online" && (
                            <>
                              <button
                                type="button"
                                className="btn"
                                disabled={pickingNodeId === node.id}
                                onClick={() => handlePick(node, "folder")}
                                title="Reference a local folder without copying"
                              >
                                <FolderOpen size={15} strokeWidth={1.6} />
                                <span>Folder…</span>
                              </button>
                              <button
                                type="button"
                                className="btn"
                                disabled={pickingNodeId === node.id}
                                onClick={() => handlePick(node, "file")}
                                title="Reference a single local file without copying"
                              >
                                <FilePlus2 size={15} strokeWidth={1.6} />
                                <span>File…</span>
                              </button>
                            </>
                          )}
                        </div>
                        <button type="submit" className="btn">
                          <FolderPlus size={15} strokeWidth={1.6} />
                          <span>Add</span>
                        </button>
                      </form>
                      <div className="quick-paths">
                        <span className="field-hint">Quick fill:</span>
                        {["Desktop", "Documents", "Downloads", "Pictures", "Videos"].map((folder) => (
                          <button
                            key={folder}
                            type="button"
                            className="quick-chip mono"
                            onClick={() =>
                              setMountDrafts((prev) => ({
                                ...prev,
                                [node.id]: {
                                  name: draft.name.trim() || folder,
                                  local_path: `~/${folder}`,
                                  kind: "folder",
                                },
                              }))
                            }
                          >
                            ~/{folder}
                          </button>
                        ))}
                      </div>
                    </div>
                  )}
                </div>
              </section>
            );
          })}
        </div>
      )}

      <ModalDialog open={enrollOpen} title="Connect a contributor machine" onClose={() => setEnrollOpen(false)}>
        {!enrollResult ? (
          <form className="form-stack" onSubmit={handleGenerateToken}>
            <div className="field">
              <label htmlFor="enroll-name">Machine name (optional)</label>
              <span id="enroll-name-hint" className="field-hint">
                Defaults to the computer&apos;s hostname if left blank.
              </span>
              <input
                id="enroll-name"
                type="text"
                aria-describedby="enroll-name-hint"
                value={machineName}
                onChange={(e) => setMachineName(e.target.value)}
                placeholder="e.g. songbird"
              />
            </div>
            <div className="modal-actions">
              <button type="button" className="btn" onClick={() => setEnrollOpen(false)}>
                Cancel
              </button>
              <button type="submit" className="btn btn-primary" disabled={enrollBusy}>
                {enrollBusy ? "Generating…" : "Generate enrollment command"}
              </button>
            </div>
          </form>
        ) : (
          <div className="form-stack">
            <p className="page-sub" style={{ marginTop: 0 }}>
              Paste this command into your terminal to install, enroll, and start the background agent. Token expires
              in 15 minutes.
            </p>

            <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 8, flexWrap: "wrap" }}>
              <div className="seg-control" role="group" aria-label="Install platform">
                <button
                  type="button"
                  className="seg-btn"
                  aria-pressed={installTab === "windows"}
                  onClick={() => setInstallTab("windows")}
                >
                  Windows (PowerShell)
                </button>
                <button
                  type="button"
                  className="seg-btn"
                  aria-pressed={installTab === "unix"}
                  onClick={() => setInstallTab("unix")}
                >
                  macOS / Linux
                </button>
                <button
                  type="button"
                  className="seg-btn"
                  aria-pressed={installTab === "source"}
                  onClick={() => setInstallTab("source")}
                >
                  From source
                </button>
              </div>
              <span className="tag-chip mono">
                {enrollResult.binary_source === "official" ? "Signed release" : "Served by this Hub"}
              </span>
            </div>

            {installTab !== "source" ? (
              <div className="code-block">
                <div className="code-head">
                  <span>{installTab === "windows" ? "PowerShell (Windows)" : "Terminal (macOS / Linux)"}</span>
                  <button
                    type="button"
                    className="btn"
                    onClick={() =>
                      copyText(
                        "install",
                        (installTab === "windows" ? enrollResult.install_windows : enrollResult.install_unix) ||
                          enrollResult.command
                      )
                    }
                  >
                    {copiedKey === "install" ? <Check size={14} /> : <Copy size={14} />}
                    <span>{copiedKey === "install" ? "Copied" : "Copy"}</span>
                  </button>
                </div>
                <pre className="mono" style={{ whiteSpace: "pre-wrap", wordBreak: "break-all" }}>
                  {(installTab === "windows" ? enrollResult.install_windows : enrollResult.install_unix) ||
                    enrollResult.command}
                </pre>
              </div>
            ) : (
              <>
                <div className="code-block">
                  <div className="code-head">
                    <span>1. Enroll machine</span>
                    <button
                      type="button"
                      className="btn"
                      onClick={() => copyText("enroll", enrollResult.command)}
                    >
                      {copiedKey === "enroll" ? <Check size={14} /> : <Copy size={14} />}
                      <span>{copiedKey === "enroll" ? "Copied" : "Copy"}</span>
                    </button>
                  </div>
                  <pre className="mono">{enrollResult.command}</pre>
                </div>

                <div className="code-block">
                  <div className="code-head">
                    <span>2. Start headless agent</span>
                    <button
                      type="button"
                      className="btn"
                      onClick={() => copyText("run", "go run ./cmd/agent run")}
                    >
                      {copiedKey === "run" ? <Check size={14} /> : <Copy size={14} />}
                      <span>{copiedKey === "run" ? "Copied" : "Copy"}</span>
                    </button>
                  </div>
                  <pre className="mono">go run ./cmd/agent run</pre>
                </div>
              </>
            )}

            <div className="modal-actions">
              <button
                type="button"
                className="btn btn-primary"
                onClick={() => {
                  setEnrollOpen(false);
                  void onRefresh();
                }}
              >
                Done
              </button>
            </div>
          </div>
        )}
      </ModalDialog>
    </div>
  );
}

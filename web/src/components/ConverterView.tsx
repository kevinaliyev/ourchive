/**
 * ConverterView — 3-tier media & document converter (Agent GPU -> browser Canvas/WebCodecs -> offline).
 * Supports full-screen drag-and-drop, mixed-type batch queues with per-file target formats,
 * pre-conversion size estimation, live progress, auto-scroll to output, and 1-click host share links.
 */
import { useCallback, useEffect, useRef, useState } from "react";
import { ArrowLeftRight, Check, Cpu, Download, ExternalLink, FileUp, Info, Link2, Trash2, Upload } from "lucide-react";
import { api, ComputeJob, formatBytes, formatDate, NodeView, User } from "../api";
import { canConvertInBrowser, convertInBrowser } from "../converter";

interface QueuedFile {
  id: string;
  file: File;
  kind: "video" | "audio" | "image" | "doc" | "other";
  targetFormat: string;
  durationSec?: number;
  width?: number;
  height?: number;
}

function defaultFormatFor(kind: QueuedFile["kind"], filename: string): string {
  if (kind === "image") return "webp";
  if (kind === "audio") return "mp3";
  if (kind === "video") return "mp4";
  if (kind === "doc") return filename.toLowerCase().endsWith(".json") ? "csv" : "html";
  return "mp4";
}

interface ConvertedItem {
  id: string;
  originalName: string;
  originalSize: number;
  outputName: string;
  outputSize: number;
  tierLabel: string;
  encoder: string;
  durationMs: number;
  blob: Blob;
  blobUrl: string;
  shareUrl?: string;
}

interface ConverterViewProps {
  active: boolean;
  nodes: NodeView[];
  currentUser: User | null;
  onSharesChanged: () => Promise<void>;
}

type PresetId = "web_fast" | "discord_10mb" | "high_quality";

const FORMATS_BY_KIND: Record<QueuedFile["kind"], string[]> = {
  video: ["mp4", "webm", "gif", "mp3", "wav", "mkv"],
  audio: ["mp3", "m4a", "ogg", "wav", "flac"],
  image: ["webp", "jpg", "png", "avif"],
  doc: ["html", "csv", "json", "txt"],
  other: ["mp4", "mp3", "webp", "png"],
};

function detectKind(file: File): QueuedFile["kind"] {
  const mime = file.type.toLowerCase();
  const ext = file.name.slice(file.name.lastIndexOf(".") + 1).toLowerCase();
  if (mime.startsWith("video/") || ["mp4", "mkv", "mov", "webm", "avi", "m4v"].includes(ext)) return "video";
  if (mime.startsWith("audio/") || ["mp3", "wav", "flac", "ogg", "opus", "m4a"].includes(ext)) return "audio";
  if (mime.startsWith("image/") || ["png", "jpg", "jpeg", "webp", "gif", "bmp", "avif", "svg"].includes(ext)) return "image";
  if (["md", "txt", "json", "csv"].includes(ext) || mime.startsWith("text/")) return "doc";
  return "other";
}

async function probeFileInBrowser(file: File, kind: QueuedFile["kind"]): Promise<Partial<QueuedFile>> {
  if (kind === "video" || kind === "audio") {
    return new Promise((resolve) => {
      const url = URL.createObjectURL(file);
      const el = document.createElement(kind === "video" ? "video" : "audio");
      el.preload = "metadata";
      const done = (meta: Partial<QueuedFile>) => {
        URL.revokeObjectURL(url);
        resolve(meta);
      };
      const timer = window.setTimeout(() => done({}), 2500);
      el.onloadedmetadata = () => {
        window.clearTimeout(timer);
        const dur = Number.isFinite(el.duration) && el.duration > 0 ? el.duration : undefined;
        if (el instanceof HTMLVideoElement) {
          done({ durationSec: dur, width: el.videoWidth || undefined, height: el.videoHeight || undefined });
        } else {
          done({ durationSec: dur });
        }
      };
      el.onerror = () => {
        window.clearTimeout(timer);
        done({});
      };
      el.src = url;
    });
  }
  if (kind === "image") {
    try {
      const bmp = await createImageBitmap(file);
      const res = { width: bmp.width, height: bmp.height };
      bmp.close();
      return res;
    } catch {
      return {};
    }
  }
  return {};
}

interface SizeEstimate {
  bytes: number;
  note?: string;
  mayExceed10MB?: boolean;
}

function estimateOutput(item: QueuedFile, format: string, preset: PresetId): SizeEstimate {
  const orig = item.file.size;
  const dur = item.durationSec ?? 0;

  if (["mp4", "webm", "mkv", "mov"].includes(format)) {
    if (preset === "discord_10mb") {
      if (dur > 0) {
        // Minimum usable video bitrate ~200 kbps + 96 kbps audio = 296 kbps (~37 KB/s)
        const minBytes = Math.round(dur * ((200 + 96) * 125));
        const targetBytes = Math.round(9.2 * 1024 * 1024);
        if (minBytes > 10 * 1024 * 1024) {
          return {
            bytes: minBytes,
            mayExceed10MB: true,
            note: `At ${formatDuration(dur)}, fitting under 10 MB likely isn't possible without severe quality loss.`,
          };
        }
        return { bytes: Math.min(orig, targetBytes) };
      }
      return { bytes: Math.min(orig, Math.round(9.2 * 1024 * 1024)) };
    }
    if (preset === "web_fast") {
      if (dur > 0) {
        // ~2.5 Mbps video + 128k audio
        return { bytes: Math.min(orig, Math.round(dur * 330_000)) };
      }
      return { bytes: Math.round(orig * 0.6) };
    }
    // high_quality
    return { bytes: Math.round(orig * 0.92) };
  }

  if (["mp3", "m4a", "ogg"].includes(format)) {
    if (dur > 0) {
      const kbps = preset === "discord_10mb" ? 96 : preset === "high_quality" ? 256 : 160;
      return { bytes: Math.round(dur * kbps * 125) };
    }
    return { bytes: Math.round(orig * 0.25) };
  }
  if (format === "wav") {
    if (dur > 0) return { bytes: Math.round(dur * 176_400) };
    return { bytes: Math.round(orig * 3.5) };
  }
  if (format === "webp" || format === "avif" || format === "jpg") {
    const ratio = preset === "discord_10mb" ? 0.32 : preset === "high_quality" ? 0.8 : 0.48;
    return { bytes: Math.max(8 * 1024, Math.round(orig * ratio)) };
  }
  return { bytes: orig };
}

function formatDuration(sec: number): string {
  const m = Math.floor(sec / 60);
  const s = Math.round(sec % 60);
  return m > 0 ? `${m}m ${s}s` : `${s}s`;
}

export function ConverterView({ active, nodes, currentUser, onSharesChanged }: ConverterViewProps) {
  const [computeNodes, setComputeNodes] = useState<NodeView[]>([]);
  const [jobs, setJobs] = useState<ComputeJob[]>([]);
  const [queue, setQueue] = useState<QueuedFile[]>([]);
  const [preset, setPreset] = useState<PresetId>("web_fast");
  const [engineMode, setEngineMode] = useState<"auto" | "agent" | "browser">("auto");
  const [selectedNodeId, setSelectedNodeId] = useState("");
  const [globalDragActive, setGlobalDragActive] = useState(false);
  const [converting, setConverting] = useState(false);
  const [sharingId, setSharingId] = useState<string | null>(null);
  const [copiedId, setCopiedId] = useState<string | null>(null);
  const [progress, setProgress] = useState<{ done: number; total: number } | null>(null);
  const [scrollNonce, setScrollNonce] = useState(0);
  const [statusText, setStatusText] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [results, setResults] = useState<ConvertedItem[]>([]);
  const inputRef = useRef<HTMLInputElement>(null);
  const resultsRef = useRef<HTMLElement>(null);
  const dragDepthRef = useRef(0);

  useEffect(() => {
    if (scrollNonce === 0) return;
    const el = resultsRef.current;
    if (!el) return;
    const rect = el.getBoundingClientRect();
    const fullyVisible = rect.top >= 0 && rect.bottom <= window.innerHeight;
    if (!fullyVisible) {
      const reduced = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
      el.scrollIntoView({ behavior: reduced ? "auto" : "smooth", block: "end" });
    }
  }, [scrollNonce, results.length]);

  const refreshCompute = useCallback(async () => {
    try {
      const [nRes, jRes] = await Promise.all([api.listComputeNodes(), api.listComputeJobs()]);
      setComputeNodes(nRes.nodes);
      setJobs(jRes.jobs);
      if (!selectedNodeId && nRes.nodes.length > 0) {
        setSelectedNodeId(nRes.nodes[0].id);
      }
    } catch {}
  }, [selectedNodeId]);

  useEffect(() => {
    void refreshCompute();
  }, [refreshCompute]);

  const addFiles = useCallback(async (list: FileList | File[]) => {
    const arr = Array.from(list);
    if (arr.length === 0) return;

    const initial: QueuedFile[] = arr.map((f, idx) => {
      const kind = detectKind(f);
      return {
        id: `${Date.now()}-${idx}-${f.name}`,
        file: f,
        kind,
        targetFormat: defaultFormatFor(kind, f.name),
      };
    });

    setQueue((prev) => [...prev, ...initial]);

    // Probe duration / resolution asynchronously in browser for accurate size estimates.
    for (const item of initial) {
      const meta = await probeFileInBrowser(item.file, item.kind);
      if (Object.keys(meta).length > 0) {
        setQueue((prev) => prev.map((q) => (q.id === item.id ? { ...q, ...meta } : q)));
      }
    }
  }, []);

  // Full-viewport drag-and-drop overlay (active only when Converter tab is visible)
  useEffect(() => {
    if (!active) {
      dragDepthRef.current = 0;
      setGlobalDragActive(false);
      return;
    }
    const onDragEnter = (e: DragEvent) => {
      if (!e.dataTransfer?.types.includes("Files")) return;
      e.preventDefault();
      dragDepthRef.current += 1;
      setGlobalDragActive(true);
    };
    const onDragOver = (e: DragEvent) => {
      if (!e.dataTransfer?.types.includes("Files")) return;
      e.preventDefault();
    };
    const onDragLeave = (e: DragEvent) => {
      if (!e.dataTransfer?.types.includes("Files")) return;
      e.preventDefault();
      dragDepthRef.current = Math.max(0, dragDepthRef.current - 1);
      if (dragDepthRef.current === 0) setGlobalDragActive(false);
    };
    const onDrop = (e: DragEvent) => {
      if (!e.dataTransfer?.files?.length) return;
      e.preventDefault();
      dragDepthRef.current = 0;
      setGlobalDragActive(false);
      void addFiles(e.dataTransfer.files);
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
  }, [active, addFiles]);

  const activeNode = computeNodes.find((n) => n.id === selectedNodeId) ?? computeNodes[0] ?? null;
  const singleItem = queue.length === 1 ? queue[0] : null;
  const hasCompressibleItem = queue.some(
    (q) => q.kind !== "doc" && q.targetFormat !== "wav" && q.targetFormat !== "flac",
  );

  const totalInputBytes = queue.reduce((s, q) => s + q.file.size, 0);
  const estimates = {
    web_fast: queue.reduce((s, q) => s + estimateOutput(q, q.targetFormat, "web_fast").bytes, 0),
    discord_10mb: queue.reduce((s, q) => s + estimateOutput(q, q.targetFormat, "discord_10mb").bytes, 0),
    high_quality: queue.reduce((s, q) => s + estimateOutput(q, q.targetFormat, "high_quality").bytes, 0),
  };
  const activeWarning = queue
    .map((q) => estimateOutput(q, q.targetFormat, preset).note)
    .find(Boolean);

  function setItemFormat(id: string, nextFmt: string) {
    setQueue((prev) => prev.map((q) => (q.id === id ? { ...q, targetFormat: nextFmt } : q)));
  }

  async function handleConvertAll() {
    if (queue.length === 0) return;
    setConverting(true);
    setProgress({ done: 0, total: queue.length });
    setError(null);

    const newResults: ConvertedItem[] = [];
    for (let i = 0; i < queue.length; i++) {
      const { file: f, targetFormat: fmt } = queue[i];
      const browserCapable = canConvertInBrowser(f, fmt);
      const useBrowser =
        engineMode === "browser" || (engineMode === "auto" && browserCapable && preset !== "discord_10mb");

      try {
        if (useBrowser) {
          if (!browserCapable) {
            throw new Error(`Browser local mode can't encode .${fmt} from ${f.name} — use Agent GPU instead.`);
          }
          setStatusText(`Converting ${f.name} → .${fmt} locally (${i + 1}/${queue.length})…`);
          const res = await convertInBrowser(f, fmt, preset);
          newResults.push({
            id: `${Date.now()}-${i}`,
            originalName: f.name,
            originalSize: f.size,
            outputName: res.filename,
            outputSize: res.blob.size,
            tierLabel: res.tierLabel,
            encoder: res.encoder,
            durationMs: res.durationMs,
            blob: res.blob,
            blobUrl: URL.createObjectURL(res.blob),
          });
        } else {
          if (!activeNode) {
            throw new Error(
              "No online GPU/ffmpeg Agent is connected. Start your agent or choose a browser-supported image/WAV/document format.",
            );
          }
          setStatusText(`Encoding ${f.name} → .${fmt} on ${activeNode.display_name} (${i + 1}/${queue.length})…`);
          const res = await api.convertOnAgent(activeNode.id, f, fmt, preset);
          newResults.push({
            id: `${Date.now()}-${i}`,
            originalName: f.name,
            originalSize: f.size,
            outputName: res.filename,
            outputSize: res.blob.size,
            tierLabel: `Agent GPU (${activeNode.display_name})`,
            encoder: res.encoder,
            durationMs: res.durationMs,
            blob: res.blob,
            blobUrl: URL.createObjectURL(res.blob),
          });
        }
        setProgress({ done: i + 1, total: queue.length });
      } catch (err) {
        setError(err instanceof Error ? err.message : `Failed to convert ${f.name}`);
        break;
      }
    }

    if (newResults.length > 0) {
      setResults((prev) => [...newResults, ...prev]);
      setScrollNonce((n) => n + 1);
    }
    setStatusText(null);
    setProgress(null);
    setConverting(false);
    void refreshCompute();
  }

  // Find an online folder on the user's own host machine to store temporary converted shares.
  const ownOnlineHost = nodes.find(
    (n) =>
      n.status === "online" &&
      (currentUser ? n.owner_id === currentUser.id : n.is_owner) &&
      n.mounts.some((m) => m.enabled && !m.rejected && m.kind !== "file"),
  );
  const ownShareMount = ownOnlineHost?.mounts.find((m) => m.enabled && !m.rejected && m.kind !== "file");

  async function handleShareConverted(item: ConvertedItem) {
    if (!ownOnlineHost || !ownShareMount) {
      setError("Connect your machine with at least one shared folder to host temporary share links from the Converter.");
      return;
    }
    setError(null);
    setSharingId(item.id);
    try {
      const relPath = `_converted/${item.outputName}`;
      const fileObj = new File([item.blob], item.outputName, { type: item.blob.type || "application/octet-stream" });
      await api.uploadFile(ownOnlineHost.id, ownShareMount.id, relPath, fileObj);
      const res = await api.createShare({
        node_id: ownOnlineHost.id,
        mount_id: ownShareMount.id,
        rel_path: relPath,
        is_dir: false,
        title: item.outputName,
        mime_type: item.blob.type || "application/octet-stream",
        size_bytes: item.outputSize,
        expires_in_hours: 24,
      });
      setResults((prev) => prev.map((r) => (r.id === item.id ? { ...r, shareUrl: res.share.url } : r)));
      try {
        await navigator.clipboard.writeText(res.share.url);
        setCopiedId(item.id);
        setTimeout(() => setCopiedId(null), 2000);
      } catch {}
      await onSharesChanged();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not create share link");
    } finally {
      setSharingId(null);
    }
  }

  return (
    <div className="section-stack">
      {/* Full-Screen Dimmed Drag-and-Drop Overlay */}
      {globalDragActive && (
        <div className="fullscreen-drop-overlay" aria-live="polite">
          <div className="fullscreen-drop-card">
            <FileUp size={28} strokeWidth={1.5} />
            <div className="empty-title">Drop files anywhere to convert</div>
            <div className="node-sub">Release to add to your conversion queue</div>
          </div>
        </div>
      )}

      <input
        ref={inputRef}
        type="file"
        multiple
        hidden
        onChange={(e) => {
          if (e.target.files) void addFiles(e.target.files);
          e.target.value = "";
        }}
      />

      {/* Step 1: Calm Drop Stage (when queue is empty) */}
      {queue.length === 0 ? (
        <section
          className="card empty-card"
          style={{ cursor: "pointer", padding: "56px 24px" }}
          onClick={() => inputRef.current?.click()}
        >
          <Upload size={26} strokeWidth={1.4} className="muted-icon" />
          <div className="empty-title">Drop any file anywhere on this screen</div>
          <p className="empty-copy">
            Or click to browse. We&apos;ll inspect your file first and estimate output sizes before converting.
          </p>
          <button
            type="button"
            className="btn btn-primary"
            onClick={(e) => {
              e.stopPropagation();
              inputRef.current?.click();
            }}
          >
            Choose files
          </button>
          <div className="node-sub" style={{ marginTop: 12 }}>
            <span className={`dot ${activeNode ? "" : "offline"}`} />
            <span>
              {activeNode
                ? `GPU ready on ${activeNode.display_name}${activeNode.compute.hw_encoders?.length ? ` (${activeNode.compute.hw_encoders[0]})` : ""}`
                : "Browser local mode active (images, WAV, docs)"}
            </span>
          </div>
        </section>
      ) : (
        /* Step 2: Progressive, Uncluttered Conversion Controls */
        <section className="card section-stack">
          <div className="toolbar-row">
            <div>
              <h2 className="section-title">
                {queue.length === 1 ? queue[0].file.name : `${queue.length} files selected`}
              </h2>
              <div className="node-sub mono">
                Original: {formatBytes(totalInputBytes)}
                {queue.length === 1 && queue[0].durationSec ? ` · ${formatDuration(queue[0].durationSec)}` : ""}
                {queue.length === 1 && queue[0].width ? ` · ${queue[0].width}×${queue[0].height}` : ""}
              </div>
            </div>
            <div className="row-actions">
              <button type="button" className="btn btn-sm" onClick={() => inputRef.current?.click()}>
                Add more
              </button>
              <button type="button" className="btn btn-sm" onClick={() => setQueue([])}>
                Clear
              </button>
            </div>
          </div>

          {queue.length > 1 ? (
            <ul className="mount-list">
              {queue.map((item) => {
                const opts = FORMATS_BY_KIND[item.kind] ?? FORMATS_BY_KIND.other;
                const est = estimateOutput(item, item.targetFormat, preset);
                return (
                  <li key={item.id} className="mount-item">
                    <div style={{ minWidth: 0 }}>
                      <div className="mount-name">{item.file.name}</div>
                      <div className="mount-path mono">
                        {formatBytes(item.file.size)} → ~Est. {formatBytes(est.bytes)}
                        {item.durationSec ? ` · ${formatDuration(item.durationSec)}` : ""}
                      </div>
                    </div>
                    <div className="row-actions">
                      <select
                        className="sort-select mono"
                        aria-label={`Target format for ${item.file.name}`}
                        value={item.targetFormat}
                        onChange={(e) => setItemFormat(item.id, e.target.value)}
                      >
                        {opts.map((opt) => (
                          <option key={opt} value={opt}>
                            .{opt}
                          </option>
                        ))}
                      </select>
                      <button
                        type="button"
                        className="icon-btn danger"
                        aria-label={`Remove ${item.file.name}`}
                        onClick={() => setQueue((prev) => prev.filter((q) => q.id !== item.id))}
                      >
                        <Trash2 size={14} strokeWidth={1.5} />
                      </button>
                    </div>
                  </li>
                );
              })}
            </ul>
          ) : (
            singleItem && (
              <div className="field">
                <label>Convert to</label>
                <div className="filter-chips" role="group" aria-label="Target format">
                  {(FORMATS_BY_KIND[singleItem.kind] ?? FORMATS_BY_KIND.other).map((opt) => (
                    <button
                      key={opt}
                      type="button"
                      className="filter-chip mono"
                      aria-pressed={singleItem.targetFormat === opt}
                      onClick={() => setItemFormat(singleItem.id, opt)}
                    >
                      .{opt}
                    </button>
                  ))}
                </div>
              </div>
            )
          )}

          {/* Honest Compression Options with Estimated Result Size */}
          {hasCompressibleItem && (
            <div className="field">
              <label>Compression level (estimated output)</label>
              <div className="preset-cards">
                <button
                  type="button"
                  className="preset-card"
                  aria-pressed={preset === "web_fast"}
                  onClick={() => setPreset("web_fast")}
                >
                  <div className="preset-card-top">
                    <strong>Balanced</strong>
                    <span className="tag-chip mono">~Est. {formatBytes(estimates.web_fast)}</span>
                  </div>
                  <small>1080p max, good visual quality, fast web playback.</small>
                </button>

                <button
                  type="button"
                  className="preset-card"
                  aria-pressed={preset === "discord_10mb"}
                  onClick={() => setPreset("discord_10mb")}
                >
                  <div className="preset-card-top">
                    <strong>Small / Chat target</strong>
                    <span className="tag-chip mono">~Est. {formatBytes(estimates.discord_10mb)}</span>
                  </div>
                  <small>Aims under 10 MB for Discord/chat (720p max). Target only, not guaranteed.</small>
                </button>

                <button
                  type="button"
                  className="preset-card"
                  aria-pressed={preset === "high_quality"}
                  onClick={() => setPreset("high_quality")}
                >
                  <div className="preset-card-top">
                    <strong>High quality</strong>
                    <span className="tag-chip mono">~Est. {formatBytes(estimates.high_quality)}</span>
                  </div>
                  <small>Minimal compression, preserves detail closest to original.</small>
                </button>
              </div>

              <div className="estimate-note">
                <Info size={13} strokeWidth={1.6} />
                <span>
                  Sizes are estimates based on duration and target bitrate — actual size varies by motion and scene
                  detail.
                </span>
              </div>
              {activeWarning && (
                <div className="alert-error" role="status" style={{ marginTop: 6 }}>
                  {activeWarning}
                </div>
              )}
            </div>
          )}

          {/* Optional Advanced Engine Override (collapsed by default to keep UI uncluttered) */}
          <details className="advanced-details">
            <summary>
              Engine: {engineMode === "auto" ? "Automatic" : engineMode === "agent" ? "Agent GPU" : "Browser local"}
              {activeNode && engineMode !== "browser" ? ` · ${activeNode.display_name}` : ""}
            </summary>
            <div className="converter-controls">
              <div className="field">
                <label htmlFor="conv-engine">Execution tier</label>
                <select
                  id="conv-engine"
                  value={engineMode}
                  onChange={(e) => setEngineMode(e.target.value as "auto" | "agent" | "browser")}
                >
                  <option value="auto">Automatic (Browser for images/docs · Agent GPU for video/audio)</option>
                  <option value="agent">Agent GPU ffmpeg</option>
                  <option value="browser">Browser local only (0 network)</option>
                </select>
              </div>
              {computeNodes.length > 0 && engineMode !== "browser" && (
                <div className="field">
                  <label htmlFor="conv-node">GPU host machine</label>
                  <select id="conv-node" value={activeNode?.id ?? ""} onChange={(e) => setSelectedNodeId(e.target.value)}>
                    {computeNodes.map((n) => (
                      <option key={n.id} value={n.id}>
                        {n.display_name}
                        {n.compute.hw_encoders?.length ? ` (${n.compute.hw_encoders.join(", ")})` : " (CPU)"}
                      </option>
                    ))}
                  </select>
                </div>
              )}
            </div>
          </details>

          {error && (
            <div className="alert-error" role="alert">
              {error}
            </div>
          )}

          <div className="convert-footer">
            {converting ? (
              <div className="convert-progress-wrap" role="status" aria-live="polite">
                <div className="convert-progress-meta">
                  <span>{statusText || "Converting…"}</span>
                  {progress && (
                    <span className="mono">
                      {progress.done}/{progress.total}
                    </span>
                  )}
                </div>
                <div className="convert-progress-track">
                  <div
                    className={`convert-progress-bar ${progress && progress.done === 0 ? "indeterminate" : ""}`}
                    style={
                      progress && progress.done > 0
                        ? { width: `${Math.max(12, Math.round((progress.done / progress.total) * 100))}%` }
                        : undefined
                    }
                  />
                </div>
              </div>
            ) : (
              <span />
            )}

            <button
              type="button"
              className="btn btn-primary btn-convert-lg"
              disabled={converting}
              onClick={handleConvertAll}
            >
              <ArrowLeftRight size={16} strokeWidth={1.6} />
              <span>{converting ? "Converting…" : "Convert"}</span>
            </button>
          </div>
        </section>
      )}

      {/* Converted Outputs */}
      {results.length > 0 && (
        <section ref={resultsRef} className="card">
          <h2 className="section-title" style={{ marginBottom: 12 }}>
            Converted files
          </h2>
          <ul className="mount-list">
            {results.map((r) => {
              const delta = r.originalSize > 0 ? Math.round(((r.outputSize - r.originalSize) / r.originalSize) * 100) : 0;
              return (
                <li key={r.id} className="mount-item">
                  <div>
                    <div className="node-title-row">
                      <span className="mount-name">{r.outputName}</span>
                      <span className="tag-chip mono">{r.tierLabel}</span>
                      <span className="tag-chip mono">
                        <Cpu size={11} style={{ marginRight: 4, verticalAlign: "middle" }} />
                        {r.encoder} · {r.durationMs < 1000 ? `${r.durationMs}ms` : `${(r.durationMs / 1000).toFixed(1)}s`}
                      </span>
                    </div>
                    <div className="mount-path mono">
                      {formatBytes(r.originalSize)} → {formatBytes(r.outputSize)} ({delta > 0 ? `+${delta}%` : `${delta}%`})
                    </div>
                  </div>
                  <div className="row-actions">
                    {r.shareUrl ? (
                      <>
                        <button
                          type="button"
                          className="btn btn-sm"
                          onClick={async () => {
                            try {
                              await navigator.clipboard.writeText(r.shareUrl!);
                              setCopiedId(r.id);
                              setTimeout(() => setCopiedId(null), 1800);
                            } catch {}
                          }}
                        >
                          {copiedId === r.id ? <Check size={13} /> : <Link2 size={13} strokeWidth={1.6} />}
                          <span>{copiedId === r.id ? "Copied link" : "Copy link"}</span>
                        </button>
                        <a
                          className="icon-btn"
                          href={r.shareUrl}
                          target="_blank"
                          rel="noopener noreferrer"
                          title="Open share link"
                        >
                          <ExternalLink size={14} strokeWidth={1.6} />
                        </a>
                      </>
                    ) : (
                      ownOnlineHost && (
                        <button
                          type="button"
                          className="btn btn-sm"
                          disabled={sharingId === r.id}
                          title={`Save to ${ownOnlineHost.display_name}/_converted and create a 24h share link`}
                          onClick={() => handleShareConverted(r)}
                        >
                          <Link2 size={13} strokeWidth={1.6} />
                          <span>{sharingId === r.id ? "Creating…" : "Share link"}</span>
                        </button>
                      )
                    )}
                    <a className="btn btn-primary btn-sm" href={r.blobUrl} download={r.outputName}>
                      <Download size={13} strokeWidth={1.6} />
                      <span>Save</span>
                    </a>
                  </div>
                </li>
              );
            })}
          </ul>
        </section>
      )}

      {/* Recent GPU Jobs */}
      {jobs.length > 0 && (
        <details className="card">
          <summary style={{ cursor: "pointer", fontWeight: 500 }}>Recent GPU jobs ({jobs.length})</summary>
          <div className="table-wrap" style={{ marginTop: 12 }}>
            <table className="data-table">
              <thead>
                <tr>
                  <th>Host</th>
                  <th>Format</th>
                  <th>Preset</th>
                  <th>Status</th>
                  <th>Time</th>
                </tr>
              </thead>
              <tbody>
                {jobs.map((j) => (
                  <tr key={j.id}>
                    <td>{j.node_name}</td>
                    <td className="mono">.{j.output_format}</td>
                    <td className="mono">{j.preset || "standard"}</td>
                    <td>
                      <span className={`tag-chip mono ${j.status === "failed" ? "reject-badge" : ""}`}>{j.status}</span>
                    </td>
                    <td className="mono">{formatDate(j.created_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </details>
      )}
    </div>
  );
}

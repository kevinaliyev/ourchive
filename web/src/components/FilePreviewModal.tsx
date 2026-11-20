/**
 * FilePreviewModal — Native `<dialog>` rich file previewer supporting Range-streamed video,
 * audio, click-to-zoom images, embedded PDFs, and text/code/markdown with keyboard navigation.
 */
import { useEffect, useId, useRef, useState } from "react";
import {
  Check,
  ChevronLeft,
  ChevronRight,
  Copy,
  Download,
  File as FileIcon,
  FolderOpen,
  Info,
  Link2,
  Music,
  X,
  ZoomIn,
  ZoomOut,
} from "lucide-react";
import { api, FileEntry, formatBytes } from "../api";

export interface PreviewTarget {
  nodeId: string;
  nodeName: string;
  mountId: string;
  entry: FileEntry;
}

interface FilePreviewModalProps {
  target: PreviewTarget | null;
  isOwnHost?: boolean;
  hasPrev?: boolean;
  hasNext?: boolean;
  existingShareUrl?: string;
  onPrev?: () => void;
  onNext?: () => void;
  onInspect?: () => void;
  onShare?: () => void;
  onRevealLocal?: () => void;
  onClose: () => void;
}

const MAX_TEXT_PREVIEW_BYTES = 1024 * 1024; // 1 MB

function isTextLike(mime: string, name: string): boolean {
  if (mime.startsWith("text/") || mime === "application/json" || mime === "application/xml") return true;
  const lower = name.toLowerCase();
  return /\.(md|txt|json|yaml|yml|toml|ini|go|ts|tsx|js|jsx|css|html|sql|sh|ps1|py|rs|c|h|cpp|log|env|gitignore)$/.test(
    lower,
  );
}

export function FilePreviewModal({
  target,
  isOwnHost,
  hasPrev,
  hasNext,
  existingShareUrl,
  onPrev,
  onNext,
  onInspect,
  onShare,
  onRevealLocal,
  onClose,
}: FilePreviewModalProps) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const titleId = useId();

  const [zoomed, setZoomed] = useState(false);
  const [videoError, setVideoError] = useState(false);
  const [textContent, setTextContent] = useState<string | null>(null);
  const [textLoading, setTextLoading] = useState(false);
  const [textTruncated, setTextTruncated] = useState(false);
  const [copied, setCopied] = useState(false);
  const [copiedShare, setCopiedShare] = useState(false);

  useEffect(() => {
    const dlg = dialogRef.current;
    if (!dlg) return;
    if (target && !dlg.open) {
      dlg.showModal();
    } else if (!target && dlg.open) {
      dlg.close();
    }
  }, [target]);

  // Safari fallback for <dialog closedby="any">
  useEffect(() => {
    const dlg = dialogRef.current;
    if (!dlg || "closedBy" in HTMLDialogElement.prototype) return;
    const handleClick = (event: MouseEvent) => {
      if (event.target !== dlg) return;
      const rect = dlg.getBoundingClientRect();
      const inside =
        rect.top <= event.clientY &&
        event.clientY <= rect.top + rect.height &&
        rect.left <= event.clientX &&
        event.clientX <= rect.left + rect.width;
      if (!inside) dlg.close();
    };
    dlg.addEventListener("click", handleClick);
    return () => dlg.removeEventListener("click", handleClick);
  }, []);

  // Keyboard arrow navigation between sibling files
  useEffect(() => {
    if (!target) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "ArrowLeft" && hasPrev && onPrev) {
        e.preventDefault();
        onPrev();
      } else if (e.key === "ArrowRight" && hasNext && onNext) {
        e.preventDefault();
        onNext();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [target, hasPrev, hasNext, onPrev, onNext]);

  // Reset per-file state and fetch text content when previewing code/markdown/text
  useEffect(() => {
    setZoomed(false);
    setVideoError(false);
    setTextContent(null);
    setTextTruncated(false);
    setCopied(false);

    if (!target) return;
    if (!isTextLike(target.entry.mime_type, target.entry.name)) return;

    const controller = new AbortController();
    setTextLoading(true);
    const url = api.fileUrl(target.nodeId, target.mountId, target.entry.path);
    const headers: HeadersInit =
      target.entry.size > MAX_TEXT_PREVIEW_BYTES ? { Range: `bytes=0-${MAX_TEXT_PREVIEW_BYTES - 1}` } : {};

    fetch(url, { credentials: "same-origin", headers, signal: controller.signal })
      .then((res) => {
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        if (target.entry.size > MAX_TEXT_PREVIEW_BYTES) setTextTruncated(true);
        return res.text();
      })
      .then((txt) => setTextContent(txt))
      .catch(() => {
        if (!controller.signal.aborted) setTextContent(null);
      })
      .finally(() => {
        if (!controller.signal.aborted) setTextLoading(false);
      });

    return () => controller.abort();
  }, [target]);

  const fileUrl = target ? api.fileUrl(target.nodeId, target.mountId, target.entry.path) : "";
  const downloadUrl = target ? api.fileUrl(target.nodeId, target.mountId, target.entry.path, true) : "";
  const thumbUrl = target ? api.thumbUrl(target.nodeId, target.mountId, target.entry.path) : "";

  const mime = target?.entry.mime_type ?? "";
  const isVideo = mime.startsWith("video/");
  const isAudio = mime.startsWith("audio/");
  const isImage = mime.startsWith("image/");
  const isPdf = mime === "application/pdf";
  const isText = target ? isTextLike(mime, target.entry.name) : false;

  const prefersReducedMotion =
    typeof window !== "undefined" && window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  async function copyText() {
    if (!textContent) return;
    try {
      await navigator.clipboard.writeText(textContent);
      setCopied(true);
      setTimeout(() => setCopied(false), 1800);
    } catch {}
  }

  return (
    <dialog
      ref={dialogRef}
      className="preview-dialog"
      aria-labelledby={titleId}
      onClose={onClose}
      {...({ closedby: "any" } as Record<string, string>)}
    >
      {target && (
        <div className="preview-shell">
          <header className="preview-head">
            <div className="preview-title-group">
              <h2 id={titleId} className="preview-title">
                {target.entry.name}
              </h2>
              <span className="preview-meta mono">
                {target.nodeName} · {formatBytes(target.entry.size)}
                {(isVideo || isAudio) && " · Ready to stream"}
              </span>
            </div>

            <div className="preview-actions">
              {hasPrev && onPrev && (
                <button type="button" className="icon-btn" aria-label="Previous file" onClick={onPrev}>
                  <ChevronLeft size={16} strokeWidth={1.6} />
                </button>
              )}
              {hasNext && onNext && (
                <button type="button" className="icon-btn" aria-label="Next file" onClick={onNext}>
                  <ChevronRight size={16} strokeWidth={1.6} />
                </button>
              )}
              {isImage && (
                <button
                  type="button"
                  className="icon-btn"
                  aria-label={zoomed ? "Fit to screen" : "Zoom image"}
                  onClick={() => setZoomed((z) => !z)}
                >
                  {zoomed ? <ZoomOut size={16} strokeWidth={1.6} /> : <ZoomIn size={16} strokeWidth={1.6} />}
                </button>
              )}
              {isText && textContent !== null && (
                <button type="button" className="btn btn-sm" onClick={copyText}>
                  {copied ? <Check size={13} /> : <Copy size={13} />}
                  <span>{copied ? "Copied" : "Copy"}</span>
                </button>
              )}
              {onInspect && (
                <button type="button" className="icon-btn" aria-label="File details" onClick={onInspect}>
                  <Info size={16} strokeWidth={1.6} />
                </button>
              )}
              {existingShareUrl ? (
                <button
                  type="button"
                  className="btn btn-sm"
                  onClick={async () => {
                    try {
                      await navigator.clipboard.writeText(existingShareUrl);
                      setCopiedShare(true);
                      setTimeout(() => setCopiedShare(false), 1800);
                    } catch {}
                  }}
                >
                  {copiedShare ? <Check size={13} /> : <Link2 size={13} strokeWidth={1.6} />}
                  <span>{copiedShare ? "Copied link" : "Copy share link"}</span>
                </button>
              ) : (
                onShare && (
                  <button type="button" className="btn btn-sm" onClick={onShare}>
                    <Link2 size={13} strokeWidth={1.6} />
                    <span>Create share link</span>
                  </button>
                )
              )}
              {isOwnHost ? (
                onRevealLocal && (
                  <button type="button" className="btn btn-sm" onClick={onRevealLocal}>
                    <FolderOpen size={13} strokeWidth={1.6} />
                    <span>Show on PC</span>
                  </button>
                )
              ) : (
                <a className="btn btn-primary btn-sm" href={downloadUrl}>
                  <Download size={13} strokeWidth={1.6} />
                  <span>Download</span>
                </a>
              )}
              <button
                type="button"
                className="icon-btn"
                aria-label="Close preview"
                autoFocus
                onClick={() => dialogRef.current?.close()}
              >
                <X size={16} strokeWidth={1.6} />
              </button>
            </div>
          </header>

          <div className={`preview-stage ${isImage || isVideo ? "dark-stage" : ""}`}>
            {isVideo &&
              (!videoError ? (
                <video
                  key={fileUrl}
                  className="preview-video"
                  controls
                  playsInline
                  autoPlay={!prefersReducedMotion}
                  width={1280}
                  height={720}
                  preload="metadata"
                  poster={thumbUrl}
                  src={fileUrl}
                  onError={() => setVideoError(true)}
                />
              ) : (
                <div className="preview-fallback">
                  <div className="empty-title">Format not natively playable in this browser</div>
                  <p className="empty-copy">
                    Your browser can&apos;t decode <span className="mono">{target.entry.name}</span> directly (common
                    for MKV, HEVC, or ProRes files). Download it or convert it to MP4/WebM once the Converter lands in
                    Phase 5.
                  </p>
                  <a className="btn btn-primary" href={downloadUrl}>
                    <Download size={14} strokeWidth={1.6} />
                    <span>Download original</span>
                  </a>
                </div>
              ))}

            {isAudio && (
              <div className="preview-audio-stage">
                <div className="audio-badge">
                  <Music size={28} strokeWidth={1.4} />
                </div>
                <div className="empty-title">{target.entry.name}</div>
                <audio
                  key={fileUrl}
                  controls
                  autoPlay={!prefersReducedMotion}
                  preload="metadata"
                  src={fileUrl}
                  className="preview-audio"
                />
              </div>
            )}

            {isImage && (
              <div className={`preview-image-wrap ${zoomed ? "zoomed" : ""}`}>
                <img
                  key={fileUrl}
                  src={fileUrl}
                  alt={target.entry.name}
                  className={`preview-image ${zoomed ? "zoomed" : ""}`}
                  onClick={() => setZoomed((z) => !z)}
                />
              </div>
            )}

            {isPdf && <iframe key={fileUrl} src={fileUrl} title={target.entry.name} className="preview-pdf" />}

            {isText && (
              <div className="preview-text-wrap">
                {textLoading ? (
                  <div className="mounts-empty">Loading preview…</div>
                ) : textContent !== null ? (
                  <>
                    {textTruncated && (
                      <div className="status-banner" style={{ marginBottom: 12 }}>
                        Showing the first 1 MB of this file. Download to view the complete document.
                      </div>
                    )}
                    <pre className="preview-code mono">{textContent}</pre>
                  </>
                ) : (
                  <div className="mounts-empty">Could not load text preview.</div>
                )}
              </div>
            )}

            {!isVideo && !isAudio && !isImage && !isPdf && !isText && (
              <div className="preview-fallback">
                <FileIcon size={28} strokeWidth={1.4} className="muted-icon" />
                <div className="empty-title">{target.entry.name}</div>
                <p className="empty-copy">
                  No inline preview for <span className="mono">{mime}</span>. Download it directly from{" "}
                  {target.nodeName}.
                </p>
                <a className="btn btn-primary" href={downloadUrl}>
                  <Download size={14} strokeWidth={1.6} />
                  <span>Download ({formatBytes(target.entry.size)})</span>
                </a>
              </div>
            )}
          </div>
        </div>
      )}
    </dialog>
  );
}

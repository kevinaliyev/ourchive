/**
 * GetApp — "open the desktop app, or download it" for the hub website (never rendered inside the
 * app itself).
 *
 * A web page can't start a program or tell whether one is installed. What it can do is hand an
 * ourchive:// link to the browser, which opens the app if the OS has it registered. So the flow is:
 *   1. Click "Open app": the browser asks to open Ourchive (or the app just comes forward).
 *   2. If the page is still in front after a moment, the app probably isn't installed, so the
 *      download for this computer is highlighted. The download is always one click away anyway.
 * The installer itself has to be run by the user: browsers never execute downloads.
 */
import { useEffect, useRef, useState } from "react";
import { Download, Monitor } from "lucide-react";
import { api, ClientDownloadAsset, ClientDownloadInfo } from "../api";
import { openAppLink } from "../platform";

export type DesktopOS = "windows" | "macos" | "linux" | "other";

/** The computer's OS from the user agent; phones and tablets count as "other" (no desktop app). */
export function detectOS(ua: string = typeof navigator === "undefined" ? "" : navigator.userAgent): DesktopOS {
  if (/android|iphone|ipad|ipod/i.test(ua)) return "other";
  if (/windows/i.test(ua)) return "windows";
  if (/mac os x|macintosh/i.test(ua)) return "macos";
  if (/linux|x11|cros/i.test(ua)) return "linux";
  return "other";
}

/** The installer to offer first for this computer, if the hub has one. */
export function assetFor(info: ClientDownloadInfo | null, os: DesktopOS): ClientDownloadAsset | null {
  if (!info) return null;
  return info.assets.find((a) => a.os === os) ?? null;
}

function formatSize(bytes?: number): string {
  if (!bytes || bytes <= 0) return "";
  const mb = bytes / (1024 * 1024);
  return mb >= 10 ? `${Math.round(mb)} MB` : `${mb.toFixed(1)} MB`;
}

let infoPromise: Promise<ClientDownloadInfo | null> | null = null;

/** The hub's download info, fetched once per page load and shared by every GetApp on the page. */
export function useClientDownload(): { info: ClientDownloadInfo | null; loaded: boolean } {
  const [info, setInfo] = useState<ClientDownloadInfo | null>(null);
  const [loaded, setLoaded] = useState(false);
  useEffect(() => {
    let alive = true;
    infoPromise ??= api.getClientDownload().catch(() => null);
    void infoPromise.then((res) => {
      if (!alive) return;
      setInfo(res);
      setLoaded(true);
    });
    return () => {
      alive = false;
    };
  }, []);
  return { info, loaded };
}

type OpenState = "idle" | "opening" | "unsure";

/**
 * Hand `href` (an ourchive:// link) to the browser and report whether the page lost focus, which is
 * what happens when an app takes over. Resolves false after `waitMs` if nothing did.
 */
function openViaProtocol(href: string, waitMs = 1800): Promise<boolean> {
  return new Promise((resolve) => {
    let finished = false;
    const finish = (opened: boolean) => {
      if (finished) return;
      finished = true;
      window.clearTimeout(timer);
      window.removeEventListener("blur", onBlur);
      document.removeEventListener("visibilitychange", onVisibility);
      resolve(opened);
    };
    const onBlur = () => finish(true);
    const onVisibility = () => {
      if (document.hidden) finish(true);
    };
    const timer = window.setTimeout(() => finish(false), waitMs);
    window.addEventListener("blur", onBlur);
    document.addEventListener("visibilitychange", onVisibility);
    window.location.href = href;
  });
}

interface GetAppProps {
  /** Link to open. Defaults to bringing the app forward; pass an enroll link to connect a machine. */
  href?: string;
  /** Wording for the open button. */
  openLabel?: string;
  /** Shown above the buttons. */
  blurb?: string;
  /** Rendered as a bordered card (Contribution page) or inline (inside a dialog). */
  variant?: "card" | "inline";
}

export function GetApp({
  href = openAppLink,
  openLabel = "Open Ourchive app",
  blurb = "The desktop app keeps this computer connected in the background and stays out of your way in the tray.",
  variant = "card",
}: GetAppProps) {
  const { info, loaded } = useClientDownload();
  const os = detectOS();
  const asset = assetFor(info, os);
  const others = (info?.assets ?? []).filter((a) => a !== asset);
  const [state, setState] = useState<OpenState>("idle");
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  async function open() {
    setState("opening");
    const opened = await openViaProtocol(href);
    if (mounted.current) setState(opened ? "idle" : "unsure");
  }

  const hostedElsewhere = loaded && (!info || info.source === "none");
  const mobile = os === "other" && /android|iphone|ipad|ipod/i.test(navigator.userAgent);
  const body = (
    <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
      {variant === "card" && (
        <div style={{ display: "flex", gap: 10, alignItems: "center" }}>
          <Monitor size={18} strokeWidth={1.5} />
          <strong>Ourchive desktop app</strong>
          {info?.version && <span className="tag-chip mono">{info.version}</span>}
        </div>
      )}
      <p className="page-sub" style={{ margin: 0 }}>
        {blurb}
      </p>
      {mobile ? (
        <p className="field-hint" style={{ margin: 0 }}>
          The desktop app is for Windows, macOS and Linux computers.
        </p>
      ) : (
        <div style={{ display: "flex", gap: 8, flexWrap: "wrap", alignItems: "center" }}>
          <button type="button" className="btn btn-sm" onClick={() => void open()} disabled={state === "opening"}>
            <Monitor size={13} strokeWidth={1.6} />
            <span>{state === "opening" ? "Opening…" : openLabel}</span>
          </button>
          {asset && (
            <a
              className={state === "unsure" ? "btn btn-sm btn-primary" : "btn btn-sm"}
              href={asset.url}
              download={asset.filename}
              rel="noopener"
            >
              <Download size={13} strokeWidth={1.6} />
              <span>
                Download for {asset.label}
                {asset.size ? ` (${formatSize(asset.size)})` : ""}
              </span>
            </a>
          )}
        </div>
      )}
      {state === "unsure" && !mobile && (
        <p className="field-hint" role="status" style={{ margin: 0 }}>
          {asset
            ? "If nothing opened, the app probably isn't installed yet. Download it, run the installer, then use Open again."
            : "If nothing opened, the app probably isn't installed on this computer."}
        </p>
      )}
      {!mobile && !asset && hostedElsewhere && (
        <p className="field-hint" style={{ margin: 0 }}>
          This hub doesn&apos;t host the installer yet. Ask the host admin to publish a client release.
        </p>
      )}
      {!mobile && !asset && !hostedElsewhere && info && info.assets.length > 0 && os !== "other" && (
        <p className="field-hint" style={{ margin: 0 }}>
          There is no installer for your computer&apos;s system yet. Other downloads are listed below.
        </p>
      )}
      {!mobile && others.length > 0 && (
        <details>
          <summary className="field-hint" style={{ cursor: "pointer" }}>
            Other downloads
          </summary>
          <ul style={{ margin: "6px 0 0", paddingLeft: 18 }}>
            {others.map((a) => (
              <li key={a.filename}>
                <a href={a.url} download={a.filename} rel="noopener">
                  {a.label}
                </a>
                {a.size ? <span className="field-hint"> ({formatSize(a.size)})</span> : null}
              </li>
            ))}
          </ul>
        </details>
      )}
      {info?.source === "release" && info.release_url && (
        <a className="field-hint" href={info.release_url} target="_blank" rel="noopener noreferrer">
          Release notes
        </a>
      )}
    </div>
  );

  if (variant === "inline") return body;
  return (
    <section className="card" style={{ padding: "var(--sp-4)" }}>
      {body}
    </section>
  );
}

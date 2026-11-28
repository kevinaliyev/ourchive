/**
 * platform.ts — the one place the web build and the desktop-client build differ.
 *
 * The same React UI ships two ways:
 *   - web (`npm run build`): served by a hub; the page's origin *is* the hub.
 *   - client (`npm run build:client`, Vite mode "client"): bundled into the desktop app. The app's
 *     Go side answers `/api/*` by proxying to whichever hub is active, and offers a small
 *     `/_client/*` bridge for things only the app can do (switch hubs, browser sign-in, native
 *     downloads, the contributor agent).
 *
 * Components should ask this module (`isClient`, `hubOrigin()`, `openExternal()`, …) instead of
 * reading `window.location` or checking the build mode themselves.
 */
import { useSyncExternalStore } from "react";

/** True in the desktop-client build. Resolved at build time, so web bundles drop client-only code. */
export const isClient: boolean = import.meta.env.MODE === "client";

// ---- Client state (mirrors internal/clientcore.State) ----

export type AgentState = "idle" | "connecting" | "connected" | "retrying" | "failed";
export type AgentSummary = "none" | "ok" | "degraded" | "failed";

export interface AgentHubStatus {
  hub_url: string;
  node_id?: string;
  display_name?: string;
  state: AgentState;
  failures: number;
  max_retries: number;
  last_error?: string;
  next_retry_at?: string;
  connected_since?: string;
}

export interface ClientHubInfo {
  reachable: boolean;
  name?: string;
  version?: string;
  api_version: number;
  compatible: boolean;
  error?: string;
}

export interface ClientHub {
  id: string;
  name: string;
  url: string;
  added_at: string;
  active: boolean;
  signed_in: boolean;
  info?: ClientHubInfo;
}

export interface ClientDownload {
  id: string;
  filename: string;
  path: string;
  bytes: number;
  total: number;
  state: "running" | "done" | "failed";
  error?: string;
}

export interface ClientState {
  client_version: string;
  api_version: number;
  platform: { os: string; mobile: boolean; contributor: boolean; contributor_note?: string };
  hubs: ClientHub[];
  active_hub_id?: string;
  agent: AgentHubStatus[];
  agent_summary: AgentSummary;
  sign_in?: { hub_id: string; pending: boolean; error?: string; at: string };
  pending_enrollment?: { hub_url: string; hub_saved: boolean; received_at: string };
  downloads: ClientDownload[];
}

let clientState: ClientState | null = null;
const listeners = new Set<() => void>();

function setClientState(next: ClientState) {
  clientState = next;
  listeners.forEach((l) => l());
}

/** The latest client state, or null in the web build. */
export function getClientState(): ClientState | null {
  return clientState;
}

/** React hook: re-renders on every client state change. Always null in the web build. */
export function useClientState(): ClientState | null {
  return useSyncExternalStore(
    (onChange) => {
      listeners.add(onChange);
      return () => listeners.delete(onChange);
    },
    () => clientState,
    () => null,
  );
}

/** The hub the UI is talking to, in the client build. */
export function activeClientHub(): ClientHub | undefined {
  return clientState?.hubs.find((h) => h.id === clientState?.active_hub_id);
}

/**
 * The hub's public origin, for anything shown to humans or shared (invite links, labels). In the
 * web build that is the page's own origin; in the client it is the active hub's address.
 */
export function hubOrigin(): string {
  if (isClient) return activeClientHub()?.url ?? "";
  return typeof window !== "undefined" ? window.location.origin : "";
}

/** Host part of hubOrigin(), e.g. "archive.example.com". */
export function hubHost(): string {
  try {
    return new URL(hubOrigin()).host;
  } catch {
    return "";
  }
}

// ---- Bridge calls (client build only) ----

async function bridge<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: {
      "X-Ourchive-Client": "1",
      ...(init?.body && !(init.body instanceof Blob) ? { "Content-Type": "application/json" } : {}),
      ...(init?.headers ?? {}),
    },
  });
  const text = await res.text();
  const data = text ? JSON.parse(text) : {};
  if (!res.ok) throw new Error(data.error || `Request failed (${res.status})`);
  return data as T;
}

const post = <T>(path: string, body?: unknown) =>
  bridge<T>(path, { method: "POST", body: body === undefined ? undefined : JSON.stringify(body) });

export const clientBridge = {
  addHub: (url: string, name = "", force = false) => post<ClientHub>("/_client/hubs", { url, name, activate: true, force }),
  removeHub: (id: string) => bridge<{ ok: boolean }>(`/_client/hubs/${encodeURIComponent(id)}`, { method: "DELETE" }),
  activateHub: (id: string) => post<{ ok: boolean }>(`/_client/hubs/${encodeURIComponent(id)}/activate`),
  signInWithBrowser: (id: string) => post<{ ok: boolean }>(`/_client/hubs/${encodeURIComponent(id)}/sign-in`),
  retryAgent: (hubURL = "") => post<{ ok: boolean }>("/_client/agent/retry", { hub_url: hubURL }),
  enrollActiveHub: (token: string, name: string, readOnly: boolean) =>
    post<{ ok: boolean }>("/_client/agent/enroll", { token, name, read_only: readOnly }),
  answerEnrollment: (accept: boolean, name = "", readOnly = false) =>
    post<{ ok: boolean }>("/_client/agent/enrollment", { accept, name, read_only: readOnly }),
  download: (path: string, filename: string) =>
    post<{ started: boolean }>("/_client/download", { path, filename }),
  saveBlob: (blob: Blob, filename: string) =>
    bridge<{ saved: boolean; path: string }>(`/_client/save-blob?filename=${encodeURIComponent(filename)}`, {
      method: "POST",
      body: blob,
    }),
};

/** Opens an http(s) link outside the app: a new tab on the web, the system browser in the client. */
export function openExternal(url: string): void {
  if (!isClient) {
    window.open(url, "_blank", "noopener,noreferrer");
    return;
  }
  void post("/_client/open-external", { url }).catch((err) => console.warn("open-external failed", err));
}

// ---- Startup ----

/**
 * Must be awaited before the first render. In the client build it loads the initial state,
 * subscribes to changes, and routes downloads and external links to the OS (a webview would
 * otherwise navigate away from the app, or silently drop the file).
 */
export async function initPlatform(): Promise<void> {
  if (!isClient) return;
  try {
    setClientState(await bridge<ClientState>("/_client/state"));
  } catch (err) {
    console.error("client bridge unavailable", err);
  }
  const connect = () => {
    const es = new EventSource("/_client/events");
    es.addEventListener("state", (e) => setClientState(JSON.parse((e as MessageEvent<string>).data)));
    es.onerror = () => {
      es.close();
      window.setTimeout(connect, 2000);
    };
  };
  connect();
  document.addEventListener("click", interceptLinkClick, true);
}

function suggestedFilename(url: URL, anchor: HTMLAnchorElement): string {
  if (anchor.download) return anchor.download;
  const p = url.searchParams.get("path");
  const base = (p ?? url.pathname).split("/").filter(Boolean).pop() || "download";
  return url.pathname.endsWith("/zip") ? `${base}.zip` : base;
}

/** Client build: send hub file links to a native save dialog and other sites to the browser. */
function interceptLinkClick(e: MouseEvent) {
  if (e.defaultPrevented || e.button !== 0) return;
  const anchor = (e.target as Element | null)?.closest?.("a[href]") as HTMLAnchorElement | null;
  if (!anchor) return;
  const raw = anchor.getAttribute("href") ?? "";
  if (raw === "" || raw.startsWith("#")) return;

  if (raw.startsWith("blob:")) {
    e.preventDefault();
    const name = anchor.download || "download";
    void fetch(raw)
      .then((r) => r.blob())
      .then((b) => clientBridge.saveBlob(b, name))
      .catch((err) => console.error("save failed", err));
    return;
  }

  let url: URL;
  try {
    url = new URL(anchor.href, window.location.href);
  } catch {
    return;
  }
  if (url.origin === window.location.origin) {
    if (url.pathname.startsWith("/api/") || url.pathname.startsWith("/downloads/")) {
      e.preventDefault();
      void clientBridge
        .download(url.pathname + url.search, suggestedFilename(url, anchor))
        .catch((err) => console.error("download failed", err));
    }
    return;
  }
  if (url.protocol === "http:" || url.protocol === "https:") {
    e.preventDefault();
    openExternal(url.href);
  }
}

// ---- Web build: the hub side of the client's browser sign-in ----

const CLIENT_AUTH_KEY = "ourchive-client-auth";

export interface PendingClientAuth {
  challenge: string;
  state: string;
}

/**
 * The desktop app opens `https://<hub>/?client_auth=…&client_state=…` in the system browser. Keep
 * the request across sign-in redirects (SSO leaves and comes back), and tidy the address bar.
 */
export function capturePendingClientAuth(): PendingClientAuth | null {
  if (isClient || typeof window === "undefined") return null;
  try {
    const url = new URL(window.location.href);
    const challenge = url.searchParams.get("client_auth");
    const state = url.searchParams.get("client_state");
    if (challenge && state) {
      sessionStorage.setItem(CLIENT_AUTH_KEY, JSON.stringify({ challenge, state }));
      url.searchParams.delete("client_auth");
      url.searchParams.delete("client_state");
      window.history.replaceState({}, "", url.toString());
    }
    const raw = sessionStorage.getItem(CLIENT_AUTH_KEY);
    return raw ? (JSON.parse(raw) as PendingClientAuth) : null;
  } catch {
    return null;
  }
}

export function clearPendingClientAuth(): void {
  try {
    sessionStorage.removeItem(CLIENT_AUTH_KEY);
  } catch {}
}

/** Link that opens the desktop app and asks it to contribute this machine to the hub. */
export function enrollDeepLink(token: string, name?: string): string {
  const q = new URLSearchParams({ hub: hubOrigin(), token });
  if (name) q.set("name", name);
  return `ourchive://enroll?${q.toString()}`;
}

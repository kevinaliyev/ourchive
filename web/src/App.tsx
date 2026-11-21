import { useCallback, useEffect, useMemo, useState } from "react";
import { ArrowLeftRight, Folder, KeyRound, Link2, Monitor, Shield } from "lucide-react";
import { Announcement, api, AuthStatus, formatMbps, HealthPayload, NodeView, PublicNetworkConfig, ShareLink, User } from "./api";
import { AdminDevConsole } from "./components/AdminDevConsole";
import { AuthView } from "./components/AuthView";
import { ContributionView } from "./components/ContributionView";
import { ConverterView } from "./components/ConverterView";
import { ExplorerView } from "./components/ExplorerView";
import { SharesView } from "./components/SharesView";
import { StickyBannerStack, StickyMessage } from "./components/StickyBannerStack";
import { UserSettingsFlyout } from "./components/UserSettingsFlyout";
import { UsersGroupsView } from "./components/UsersGroupsView";

type Tab = "explorer" | "converter" | "shares" | "contribution" | "admin";

export function App() {
  const [tab, setTab] = useState<Tab>("explorer");
  const [scheme, setScheme] = useState<"dark" | "light">(() => {
    try {
      return localStorage.getItem("ourchive-color-scheme") === "light" ? "light" : "dark";
    } catch {
      return "dark";
    }
  });
  const [btnMotion, setBtnMotion] = useState<boolean>(() => {
    try {
      return localStorage.getItem("ourchive-btn-motion") !== "off";
    } catch {
      return true;
    }
  });
  const [hub, setHub] = useState<HealthPayload | null>(null);
  const [auth, setAuth] = useState<AuthStatus | null>(null);
  const [nodes, setNodes] = useState<NodeView[]>([]);
  const [shares, setShares] = useState<ShareLink[]>([]);
  const [netConfig, setNetConfig] = useState<PublicNetworkConfig | null>(null);
  const [announcements, setAnnouncements] = useState<Announcement[]>([]);
  const [devConsoleOpen, setDevConsoleOpen] = useState(false);
  const [dismissedIds, setDismissedIds] = useState<string[]>(() => {
    try {
      const raw = localStorage.getItem("ourchive-dismissed-announcements");
      return raw ? (JSON.parse(raw) as string[]) : [];
    } catch {
      return [];
    }
  });

  useEffect(() => {
    const meta = document.querySelector('meta[name="color-scheme"]');
    if (meta) meta.setAttribute("content", scheme);
    try {
      localStorage.setItem("ourchive-color-scheme", scheme);
    } catch {}
  }, [scheme]);

  useEffect(() => {
    document.documentElement.dataset.btnMotion = btnMotion ? "on" : "off";
    try {
      localStorage.setItem("ourchive-btn-motion", btnMotion ? "on" : "off");
    } catch {}
  }, [btnMotion]);

  const refreshNodes = useCallback(async () => {
    try {
      const res = await api.listNodes();
      setNodes(res.nodes);
    } catch {}
  }, []);

  const refreshShares = useCallback(async () => {
    try {
      const res = await api.listShares();
      setShares(res.shares);
    } catch {}
  }, []);

  const refreshNetConfig = useCallback(async () => {
    try {
      const cfg = await api.getPublicSettings();
      setNetConfig(cfg);
    } catch {}
  }, []);

  const refreshAnnouncements = useCallback(async () => {
    try {
      const res = await api.listAnnouncements(false);
      setAnnouncements(res.announcements);
    } catch {}
  }, []);

  useEffect(() => {
    api.health().then(setHub).catch(() => setHub(null));
    api.authStatus().then(setAuth).catch(() => setAuth(null));
  }, []);

  useEffect(() => {
    if (!auth?.user) return;
    void refreshNodes();
    void refreshShares();
    void refreshNetConfig();
    void refreshAnnouncements();

    // Real-time Server-Sent Events subscription for instant node traffic, status, and broadcasts.
    let es: EventSource | null = null;
    try {
      es = new EventSource("/api/events");
      es.addEventListener("nodes.updated", () => {
        void refreshNodes();
      });
      es.addEventListener("announcements.updated", () => {
        void refreshAnnouncements();
      });
      es.addEventListener("shares.updated", () => {
        void refreshShares();
      });
    } catch {}

    // Low-frequency fallback safety poll in case SSE drops behind a strict proxy.
    const timer = window.setInterval(() => {
      void refreshNodes();
      void refreshAnnouncements();
    }, 15000);
    return () => {
      window.clearInterval(timer);
      es?.close();
    };
  }, [auth?.user, refreshNodes, refreshShares, refreshNetConfig, refreshAnnouncements]);

  function handleDismissBanner(id: string) {
    setDismissedIds((prev) => {
      const next = prev.includes(id) ? prev : [...prev, id];
      try {
        localStorage.setItem("ourchive-dismissed-announcements", JSON.stringify(next));
      } catch {}
      return next;
    });
  }

  // Combine Host Admin broadcasts + contextual system notifications (Hub offline, HW accel off)
  const stickyMessages = useMemo<StickyMessage[]>(() => {
    const out: StickyMessage[] = [];
    if (auth?.user && !hub) {
      out.push({
        id: "sys-hub-offline",
        kind: "error",
        title: "Hub disconnected",
        body: "Cannot reach the Ourchive Hub API. Live file browsing and uploads are paused until connection resumes.",
        dismissible: false,
      });
    }
    if (auth?.user && tab === "converter") {
      const hasOnlineGPU = nodes.some(
        (n) => n.status === "online" && n.compute?.enabled && (n.compute.hw_encoders?.length ?? 0) > 0 && (n.is_owner || n.allow_group_compute),
      );
      const hasBrowserWebCodecs = typeof window !== "undefined" && "VideoEncoder" in window;
      if (!hasOnlineGPU && !hasBrowserWebCodecs && !dismissedIds.includes("sys-hw-accel-off")) {
        out.push({
          id: "sys-hw-accel-off",
          kind: "warning",
          title: "Hardware acceleration is off",
          body: "No online GPU host is available and browser hardware encoding (WebCodecs) is unavailable. Video conversions will run slower on CPU.",
          dismissible: true,
          actionLabel: "Contribution settings",
          onAction: () => setTab("contribution"),
        });
      }
    }
    for (const a of announcements) {
      if (a.dismissible && dismissedIds.includes(a.id)) continue;
      out.push({
        id: a.id,
        kind: a.kind,
        title: a.title || undefined,
        body: a.body,
        dismissible: a.dismissible,
      });
    }
    return out;
  }, [auth?.user, hub, tab, nodes, announcements, dismissedIds]);

  async function handleLogout() {
    await api.logout();
    setAuth((prev) => (prev ? { ...prev, user: null } : { initialized: true, user: null }));
    setNodes([]);
    setShares([]);
    setNetConfig(null);
  }

  function handleAuthenticated(user: User) {
    setAuth({ initialized: true, user });
  }

  const isAdmin = auth?.user?.role === "host_admin" || auth?.user?.role === "group_admin";
  const canOpenPeople = isAdmin || Boolean(auth?.user?.can_invite);
  const canUseDevConsole = isAdmin && Boolean(netConfig?.dev_console_enabled);

  useEffect(() => {
    if (!canUseDevConsole) {
      setDevConsoleOpen(false);
      return;
    }
    function onKeyDown(e: KeyboardEvent) {
      const isTilde = e.code === "Backquote" || e.key === "`" || e.key === "~";
      if (!isTilde) return;
      const target = e.target as HTMLElement | null;
      const isConsoleInput = Boolean(target?.classList?.contains("dev-prompt-input"));
      const tag = target?.tagName?.toLowerCase();
      const inOtherEditable =
        !isConsoleInput && (tag === "input" || tag === "textarea" || tag === "select" || target?.isContentEditable);
      if (inOtherEditable) return;
      e.preventDefault();
      e.stopPropagation();
      setDevConsoleOpen((v) => !v);
    }
    window.addEventListener("keydown", onKeyDown, true);
    return () => window.removeEventListener("keydown", onKeyDown, true);
  }, [canUseDevConsole]);
  const totalUpBps = nodes.reduce((acc, n) => acc + (n.traffic?.upload_bps ?? 0), 0);
  const totalDownBps = nodes.reduce((acc, n) => acc + (n.traffic?.download_bps ?? 0), 0);
  const onlineNodes = nodes.filter((n) => n.status === "online");
  const ownOnlineNodes = onlineNodes.filter((n) => n.is_owner);
  const activeHubName =
    hub?.hub_name || (typeof window !== "undefined" ? window.location.hostname : "Ourchive");
  const headerBandwidthTooltip = [
    `Combined live bandwidth across ${onlineNodes.length} connected ${onlineNodes.length === 1 ? "machine" : "machines"} on ${activeHubName} (↑ upload · ↓ download)`,
    ownOnlineNodes.length > 0
      ? `Includes your local ${ownOnlineNodes.length === 1 ? "machine" : "machines"}: ${ownOnlineNodes
          .map((n) => `${n.display_name} (↑ ${formatMbps(n.traffic?.upload_bps ?? 0)} · ↓ ${formatMbps(n.traffic?.download_bps ?? 0)})`)
          .join(", ")}`
      : "No local contributor machine connected from your account",
  ].join("\n");

  if (auth && !auth.user) {
    return (
      <div className="shell">
        <header className="topnav">
          <div className="topnav-inner">
            <div className="brand">
              <span className="brand-mark" aria-hidden="true">
                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
                  <path d="M4 7h16M4 12h16M4 17h10" />
                </svg>
              </span>
              <span>Ourchive</span>
            </div>
            <div className="segmented" role="group" aria-label="Color scheme">
              <button className="seg-btn" aria-pressed={scheme === "dark"} onClick={() => setScheme("dark")}>
                Dark
              </button>
              <button className="seg-btn" aria-pressed={scheme === "light"} onClick={() => setScheme("light")}>
                Light
              </button>
            </div>
          </div>
        </header>
        <AuthView
          initialized={auth.initialized}
          identityMode={auth.auth_identity_mode ?? "email_or_username"}
          onAuthenticated={handleAuthenticated}
        />
      </div>
    );
  }

  return (
    <div className="shell">
      <header className="topnav">
        <div className="topnav-inner">
          <div className="brand">
            <span className="brand-mark" aria-hidden="true">
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
                <path d="M4 7h16M4 12h16M4 17h10" />
              </svg>
            </span>
            <span>Ourchive</span>
          </div>

          <nav className="nav-links" aria-label="Primary">
            <button className="nav-tab" aria-current={tab === "explorer" ? "page" : undefined} onClick={() => setTab("explorer")}>
              <Folder size={15} strokeWidth={1.5} />
              <span>Explorer</span>
            </button>
            <button className="nav-tab" aria-current={tab === "converter" ? "page" : undefined} onClick={() => setTab("converter")}>
              <ArrowLeftRight size={15} strokeWidth={1.5} />
              <span>Converter</span>
            </button>
            <button className="nav-tab" aria-current={tab === "shares" ? "page" : undefined} onClick={() => setTab("shares")}>
              <Link2 size={15} strokeWidth={1.5} />
              <span>Share links</span>
            </button>
            <button className="nav-tab" aria-current={tab === "contribution" ? "page" : undefined} onClick={() => setTab("contribution")}>
              <Monitor size={15} strokeWidth={1.5} />
              <span>Contribution</span>
            </button>
            {canOpenPeople && (
              <button className="nav-tab" aria-current={tab === "admin" ? "page" : undefined} onClick={() => setTab("admin")}>
                {isAdmin ? <Shield size={15} strokeWidth={1.5} /> : <KeyRound size={15} strokeWidth={1.5} />}
                <span>{isAdmin ? "Admin" : "Invites"}</span>
              </button>
            )}
          </nav>

          <div className="nav-actions">
            {auth?.user && (
              <UserSettingsFlyout
                user={auth.user}
                scheme={scheme}
                onChangeScheme={setScheme}
                btnMotion={btnMotion}
                onChangeBtnMotion={setBtnMotion}
                netConfig={netConfig}
                onLogout={handleLogout}
              />
            )}
          </div>
        </div>
      </header>

      <StickyBannerStack messages={stickyMessages} onDismiss={handleDismissBanner} />

      {canUseDevConsole && auth?.user && (
        <AdminDevConsole
          open={devConsoleOpen}
          onClose={() => setDevConsoleOpen(false)}
          currentUser={auth.user}
          hub={hub}
          nodes={nodes}
          onNavigateTab={setTab}
          onChangeScheme={setScheme}
          onRefreshAnnouncements={refreshAnnouncements}
          onRefreshNodes={refreshNodes}
        />
      )}

      <main className="canvas">
        <div className="page-head">
          <div>
            <h1 className="page-title">
              {tab === "explorer" && "Archive"}
              {tab === "converter" && "Converter"}
              {tab === "shares" && "Share links"}
              {tab === "contribution" && "Contribution settings"}
              {tab === "admin" && (isAdmin ? "Admin" : "Invites")}
            </h1>
            <p className="page-sub">
              {tab === "explorer" && "Browse segregated nodes or tag-composed shared trees. Preview directly, download only what you need."}
              {tab === "converter" && "Convert media using your GPU host, browser hardware, or offline WASM."}
              {tab === "shares" && "Public clip embeds routed via your isolated share subdomain."}
              {tab === "contribution" && "Configure shared folders, upload limits, and GPU participation from the browser."}
              {tab === "admin" &&
                (isAdmin
                  ? "Manage members, invite lineage, groups, audit logs, and Hub transfer & storage policies."
                  : "Mint and manage your invite keys for this Ourchive hub.")}
            </p>
          </div>

          <div className="head-metrics">
            <div className="io-pair" aria-label="Total archive traffic" title={headerBandwidthTooltip}>
              <span className="io-up">↑ {formatMbps(totalUpBps)}</span>
              <span className="io-down">↓ {formatMbps(totalDownBps)}</span>
            </div>
            <div
              className="status-pill"
              title={
                hub
                  ? `Active Hub: ${activeHubName} (v${hub.version}) — ${onlineNodes.length} of ${nodes.length} ${nodes.length === 1 ? "machine" : "machines"} connected`
                  : `Hub ${activeHubName} is unreachable`
              }
            >
              <span className={`dot ${hub ? "" : "offline"}`} />
              <span>{hub ? `${activeHubName} connected (${hub.version})` : `${activeHubName} offline`}</span>
            </div>
          </div>
        </div>

        {tab === "contribution" && <ContributionView nodes={nodes} onRefresh={refreshNodes} />}
        {tab === "admin" && auth?.user && (
          <UsersGroupsView
            currentUser={auth.user}
            netConfig={netConfig}
            onSettingsSaved={refreshNetConfig}
            onAnnouncementsChanged={refreshAnnouncements}
          />
        )}

        {tab === "explorer" && auth?.user && (
          <ExplorerView
            currentUser={auth.user}
            nodes={nodes}
            shares={shares}
            netConfig={netConfig}
            onSharesChanged={refreshShares}
            onGoToContribution={() => setTab("contribution")}
          />
        )}

        {tab === "shares" && auth?.user && (
          <SharesView
            currentUser={auth.user}
            nodes={nodes}
            netConfig={netConfig}
            shares={shares}
            onRefresh={refreshShares}
            onGoToExplorer={() => setTab("explorer")}
          />
        )}

        <div hidden={tab !== "converter"}>
          <ConverterView
            active={tab === "converter"}
            nodes={nodes}
            currentUser={auth?.user ?? null}
            onSharesChanged={refreshShares}
          />
        </div>
      </main>
    </div>
  );
}

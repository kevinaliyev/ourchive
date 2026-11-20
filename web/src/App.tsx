import { useCallback, useEffect, useState } from "react";
import { ArrowLeftRight, Folder, KeyRound, Link2, Monitor, Shield } from "lucide-react";
import { api, AuthStatus, formatMbps, HealthPayload, NodeView, PublicNetworkConfig, ShareLink, User } from "./api";
import { AuthView } from "./components/AuthView";
import { ContributionView } from "./components/ContributionView";
import { ConverterView } from "./components/ConverterView";
import { ExplorerView } from "./components/ExplorerView";
import { SharesView } from "./components/SharesView";
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

  useEffect(() => {
    api.health().then(setHub).catch(() => setHub(null));
    api.authStatus().then(setAuth).catch(() => setAuth(null));
  }, []);

  useEffect(() => {
    if (!auth?.user) return;
    void refreshNodes();
    void refreshShares();
    void refreshNetConfig();
    const timer = window.setInterval(() => void refreshNodes(), 5000);
    return () => window.clearInterval(timer);
  }, [auth?.user, refreshNodes, refreshShares, refreshNetConfig]);

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
  const totalUpBps = nodes.reduce((acc, n) => acc + (n.traffic?.upload_bps ?? 0), 0);
  const totalDownBps = nodes.reduce((acc, n) => acc + (n.traffic?.download_bps ?? 0), 0);

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
            <div className="io-pair" aria-label="Total archive traffic">
              <span className="io-up">↑ {formatMbps(totalUpBps)}</span>
              <span className="io-down">↓ {formatMbps(totalDownBps)}</span>
            </div>
            <div className="status-pill">
              <span className={`dot ${hub ? "" : "offline"}`} />
              <span>{hub ? `Hub connected (${hub.version})` : "Hub offline"}</span>
            </div>
          </div>
        </div>

        {tab === "contribution" && <ContributionView nodes={nodes} onRefresh={refreshNodes} />}
        {tab === "admin" && auth?.user && (
          <UsersGroupsView currentUser={auth.user} netConfig={netConfig} onSettingsSaved={refreshNetConfig} />
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

        {tab === "shares" && (
          <SharesView shares={shares} onRefresh={refreshShares} onGoToExplorer={() => setTab("explorer")} />
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

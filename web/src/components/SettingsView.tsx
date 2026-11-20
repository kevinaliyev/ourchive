/**
 * SettingsView — Account & theme preferences for all users, plus Host Admin controls for
 * invite-only registration identity policy, tiered audit logging, WebRTC P2P/relay guardrails,
 * and opt-in provider-agnostic edge storage (S3-compatible or Bunny.net).
 */
import { FormEvent, useEffect, useState } from "react";
import { Check, Cloud, KeyRound, Radio, Shield } from "lucide-react";
import { api, AuthIdentityMode, HubSettings, PublicNetworkConfig, User } from "../api";

interface SettingsViewProps {
  currentUser: User;
  scheme?: "dark" | "light";
  onChangeScheme?: (next: "dark" | "light") => void;
  netConfig?: PublicNetworkConfig | null;
  onSettingsSaved: () => Promise<void>;
  embeddedInAdmin?: boolean;
}

export function SettingsView({
  currentUser,
  scheme = "dark",
  onChangeScheme,
  netConfig = null,
  onSettingsSaved,
  embeddedInAdmin = false,
}: SettingsViewProps) {
  const isHostAdmin = currentUser.role === "host_admin";
  const [adminCfg, setAdminCfg] = useState<HubSettings | null>(null);
  const [stunText, setStunText] = useState("");
  const [secretDraft, setSecretDraft] = useState("");
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!isHostAdmin) return;
    api
      .getAdminSettings()
      .then((cfg) => {
        setAdminCfg(cfg);
        setStunText(cfg.stun_servers.join(", "));
      })
      .catch((err) => setError(err instanceof Error ? err.message : "Failed to load admin settings"));
  }, [isHostAdmin]);

  async function handleSave(e: FormEvent) {
    e.preventDefault();
    if (!adminCfg) return;
    setSaving(true);
    setError(null);
    setNotice(null);
    try {
      const payload: HubSettings = {
        ...adminCfg,
        stun_servers: stunText
          .split(",")
          .map((s) => s.trim())
          .filter(Boolean),
        edge_storage: {
          ...adminCfg.edge_storage,
          secret_key: secretDraft.trim() || undefined,
        },
      };
      const saved = await api.saveAdminSettings(payload);
      setAdminCfg(saved);
      setSecretDraft("");
      setNotice("Hub network & storage settings saved.");
      await onSettingsSaved();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to save settings");
    } finally {
      setSaving(false);
    }
  }

  async function handleTestEdge() {
    setTesting(true);
    setError(null);
    setNotice(null);
    try {
      await api.testEdgeStorage();
      setNotice("Edge storage connection verified (write + cleanup succeeded).");
    } catch (err) {
      setError(err instanceof Error ? err.message : "Edge storage test failed");
    } finally {
      setTesting(false);
    }
  }

  return (
    <div className="section-stack">
      {/* General User Settings (hidden when embedded inside the Host Admin panel) */}
      {!embeddedInAdmin && onChangeScheme && (
        <section className="card">
        <div className="section-head">
          <div>
            <h2 className="section-title">Account &amp; preferences</h2>
            <p className="page-sub">Your profile and local browser appearance.</p>
          </div>
          <span className="tag-chip mono">{currentUser.role}</span>
        </div>

        <div className="node-controls">
          <div>
            <strong>Signed in as</strong>
            <div className="node-sub">
              {currentUser.display_name}
              {currentUser.username ? ` · @${currentUser.username}` : ""}
              {currentUser.email ? (
                <>
                  {" "}
                  · <span className="mono">{currentUser.email}</span>
                </>
              ) : null}
            </div>
          </div>

          <div className="cap-row">
            <div>
              <strong>Theme</strong>
              <small>Warm Paper surface palette</small>
            </div>
            <div className="segmented" role="group" aria-label="Theme preference">
              <button
                type="button"
                className="seg-btn"
                aria-pressed={scheme === "dark"}
                onClick={() => onChangeScheme("dark")}
              >
                Dark
              </button>
              <button
                type="button"
                className="seg-btn"
                aria-pressed={scheme === "light"}
                onClick={() => onChangeScheme("light")}
              >
                Light
              </button>
            </div>
          </div>

          <div>
            <strong>Transfer mode</strong>
            <div className="node-sub">
              <Radio size={13} strokeWidth={1.6} />
              <span>
                {netConfig?.p2p_enabled
                  ? netConfig.allow_relay_fallback
                    ? "WebRTC P2P direct (with Hub relay fallback)"
                    : "WebRTC P2P direct only (Hub relay off)"
                  : "Hub relay streaming"}
              </span>
            </div>
          </div>
        </div>
        </section>
      )}

      {/* Host Admin Only: Network Policy & Opt-In Edge Storage */}
      {isHostAdmin && adminCfg && (
        <form className="section-stack" onSubmit={handleSave}>
          {notice && (
            <div className="status-banner" role="status">
              <Check size={14} strokeWidth={1.8} />
              <span>{notice}</span>
            </div>
          )}
          {error && (
            <div className="alert-error" role="alert">
              {error}
            </div>
          )}

          <section className="card">
            <div className="section-head">
              <div>
                <h2 className="section-title">
                  <KeyRound size={16} strokeWidth={1.6} />
                  <span>Host Admin · Registration, Invites &amp; Audit Policy</span>
                </h2>
                <p className="page-sub">
                  Control invite-only sign-up identifiers, delegate invite permissions, and configure tiered audit
                  logging. Global audit switches supersede group-level settings.
                </p>
              </div>
            </div>

            <div className="form-stack">
              <div className="field">
                <label htmlFor="auth-identity-mode">Sign-up identifier requirement</label>
                <span className="field-hint">
                  Users can always sign in with whichever identifier (username or email) they registered with.
                </span>
                <select
                  id="auth-identity-mode"
                  value={adminCfg.auth_identity_mode}
                  onChange={(e) =>
                    setAdminCfg({ ...adminCfg, auth_identity_mode: e.target.value as AuthIdentityMode })
                  }
                >
                  <option value="email_or_username">Email or username (either, or both)</option>
                  <option value="both">Both email and username required</option>
                  <option value="username_only">Username only</option>
                  <option value="email_only">Email only</option>
                </select>
              </div>

              <div className="node-controls">
                <label className="switch-row">
                  <span>
                    <strong>Allow Group Admins to create invite keys</strong>
                    <small>Off by default — Host Admins and explicitly declared trusted users can still invite</small>
                  </span>
                  <input
                    type="checkbox"
                    checked={adminCfg.allow_group_admin_invites}
                    onChange={(e) => setAdminCfg({ ...adminCfg, allow_group_admin_invites: e.target.checked })}
                    {...({ switch: "" } as Record<string, string>)}
                  />
                </label>

                <label className="switch-row">
                  <span>
                    <strong>Audit core activity</strong>
                    <small>On by default — logins, invites, uploads, deletes, shares, and admin changes</small>
                  </span>
                  <input
                    type="checkbox"
                    checked={adminCfg.audit_core_enabled}
                    onChange={(e) => setAdminCfg({ ...adminCfg, audit_core_enabled: e.target.checked })}
                    {...({ switch: "" } as Record<string, string>)}
                  />
                </label>

                <label className="switch-row">
                  <span>
                    <strong>Audit file downloads</strong>
                    <small>Off by default — master switch; when off, groups cannot enable download auditing</small>
                  </span>
                  <input
                    type="checkbox"
                    checked={adminCfg.audit_downloads_enabled}
                    onChange={(e) => setAdminCfg({ ...adminCfg, audit_downloads_enabled: e.target.checked })}
                    {...({ switch: "" } as Record<string, string>)}
                  />
                </label>

                <label className="switch-row">
                  <span>
                    <strong>Audit file previews</strong>
                    <small>Off by default — master switch; when off, groups cannot enable preview auditing</small>
                  </span>
                  <input
                    type="checkbox"
                    checked={adminCfg.audit_previews_enabled}
                    onChange={(e) => setAdminCfg({ ...adminCfg, audit_previews_enabled: e.target.checked })}
                    {...({ switch: "" } as Record<string, string>)}
                  />
                </label>
              </div>
            </div>
          </section>

          <section className="card">
            <div className="section-head">
              <div>
                <h2 className="section-title">
                  <Shield size={16} strokeWidth={1.6} />
                  <span>Host Admin · Transfer &amp; Relay Policy</span>
                </h2>
                <p className="page-sub">
                  Keep your Hub near-zero egress by preferring direct WebRTC P2P and capping or disabling yamux relay
                  fallback.
                </p>
              </div>
            </div>

            <div className="form-stack">
              <div className="node-controls">
                <label className="switch-row">
                  <span>
                    <strong>WebRTC P2P direct transfers</strong>
                    <small>Stream downloads directly PC-to-Browser (0 bytes on Hub)</small>
                  </span>
                  <input
                    type="checkbox"
                    checked={adminCfg.p2p_enabled}
                    onChange={(e) => setAdminCfg({ ...adminCfg, p2p_enabled: e.target.checked })}
                    {...({ switch: "" } as Record<string, string>)}
                  />
                </label>

                <label className="switch-row">
                  <span>
                    <strong>Allow Hub relay fallback</strong>
                    <small>Fall back to yamux relay if strict NAT blocks P2P</small>
                  </span>
                  <input
                    type="checkbox"
                    checked={adminCfg.allow_relay_fallback}
                    onChange={(e) => setAdminCfg({ ...adminCfg, allow_relay_fallback: e.target.checked })}
                    {...({ switch: "" } as Record<string, string>)}
                  />
                </label>

                <label className="switch-row">
                  <span>
                    <strong>Allow Hub relay for public share embeds</strong>
                    <small>Serve temporary Discord/chat embed links via Hub</small>
                  </span>
                  <input
                    type="checkbox"
                    checked={adminCfg.allow_share_relay}
                    onChange={(e) => setAdminCfg({ ...adminCfg, allow_share_relay: e.target.checked })}
                    {...({ switch: "" } as Record<string, string>)}
                  />
                </label>
              </div>

              <div className="settings-grid-2">
                <div className="field">
                  <label htmlFor="max-relay-mb">Max relayed file size (MB)</label>
                  <span className="field-hint">0 = unlimited. Blocks huge downloads from hitting VPS egress.</span>
                  <input
                    id="max-relay-mb"
                    type="number"
                    min={0}
                    className="mono"
                    value={adminCfg.max_relay_file_mb}
                    onChange={(e) =>
                      setAdminCfg({ ...adminCfg, max_relay_file_mb: Math.max(0, Number(e.target.value) || 0) })
                    }
                  />
                </div>

                <div className="field">
                  <label htmlFor="relay-cap-mbps">Global Hub relay speed cap (Mbps)</label>
                  <span className="field-hint">0 = unlimited. Protects your VPS network interface.</span>
                  <input
                    id="relay-cap-mbps"
                    type="number"
                    min={0}
                    className="mono"
                    value={adminCfg.relay_cap_mbps}
                    onChange={(e) =>
                      setAdminCfg({ ...adminCfg, relay_cap_mbps: Math.max(0, Number(e.target.value) || 0) })
                    }
                  />
                </div>
              </div>

              <div className="field">
                <label htmlFor="stun-servers">STUN servers (comma-separated)</label>
                <input
                  id="stun-servers"
                  type="text"
                  className="mono"
                  value={stunText}
                  onChange={(e) => setStunText(e.target.value)}
                />
              </div>
            </div>
          </section>

          <section className="card">
            <div className="section-head">
              <div>
                <h2 className="section-title">
                  <Cloud size={16} strokeWidth={1.6} />
                  <span>Host Admin · Opt-In Edge / Object Storage</span>
                </h2>
                <p className="page-sub">
                  Optional provider-agnostic edge cache for public share embeds &amp; cloud syncs — supports any
                  S3-compatible storage (Cloudflare R2, Backblaze B2, MinIO, Wasabi, AWS) or Bunny.net Storage.
                </p>
              </div>
            </div>

            <div className="form-stack">
              <div className="node-controls" style={{ gridTemplateColumns: "1fr" }}>
                <label className="switch-row">
                  <span>
                    <strong>Enable opt-in edge storage</strong>
                    <small>Off by default — nothing is uploaded externally unless enabled</small>
                  </span>
                  <input
                    type="checkbox"
                    checked={adminCfg.edge_storage.enabled}
                    onChange={(e) =>
                      setAdminCfg({
                        ...adminCfg,
                        edge_storage: { ...adminCfg.edge_storage, enabled: e.target.checked },
                      })
                    }
                    {...({ switch: "" } as Record<string, string>)}
                  />
                </label>
              </div>

              <div className="field">
                <label htmlFor="edge-provider">Provider</label>
                <select
                  id="edge-provider"
                  value={adminCfg.edge_storage.provider}
                  onChange={(e) =>
                    setAdminCfg({
                      ...adminCfg,
                      edge_storage: {
                        ...adminCfg.edge_storage,
                        provider: e.target.value as "none" | "s3" | "bunny",
                      },
                    })
                  }
                >
                  <option value="none">None (disabled)</option>
                  <option value="s3">S3-Compatible (Cloudflare R2, Backblaze B2, MinIO, Wasabi, AWS)</option>
                  <option value="bunny">Bunny.net Storage + Pull Zone CDN</option>
                </select>
              </div>

              {adminCfg.edge_storage.provider !== "none" && (
                <>
                  <div className="settings-grid-2">
                    <div className="field">
                      <label htmlFor="edge-endpoint">
                        {adminCfg.edge_storage.provider === "bunny" ? "Storage host (optional)" : "S3 Endpoint URL"}
                      </label>
                      <input
                        id="edge-endpoint"
                        type="text"
                        className="mono"
                        placeholder={
                          adminCfg.edge_storage.provider === "bunny"
                            ? "storage.bunnycdn.com (or ny.storage.bunnycdn.com)"
                            : "https://<account>.r2.cloudflarestorage.com"
                        }
                        value={adminCfg.edge_storage.endpoint}
                        onChange={(e) =>
                          setAdminCfg({
                            ...adminCfg,
                            edge_storage: { ...adminCfg.edge_storage, endpoint: e.target.value },
                          })
                        }
                      />
                    </div>

                    <div className="field">
                      <label htmlFor="edge-bucket">
                        {adminCfg.edge_storage.provider === "bunny" ? "Storage Zone Name" : "Bucket Name"}
                      </label>
                      <input
                        id="edge-bucket"
                        type="text"
                        className="mono"
                        placeholder={adminCfg.edge_storage.provider === "bunny" ? "ourchive-embeds" : "my-bucket"}
                        value={adminCfg.edge_storage.bucket}
                        onChange={(e) =>
                          setAdminCfg({
                            ...adminCfg,
                            edge_storage: { ...adminCfg.edge_storage, bucket: e.target.value },
                          })
                        }
                      />
                    </div>

                    <div className="field">
                      <label htmlFor="edge-region">
                        {adminCfg.edge_storage.provider === "bunny" ? "Region prefix (e.g. de, ny, la, sg)" : "Region"}
                      </label>
                      <input
                        id="edge-region"
                        type="text"
                        className="mono"
                        placeholder={adminCfg.edge_storage.provider === "bunny" ? "de" : "auto"}
                        value={adminCfg.edge_storage.region}
                        onChange={(e) =>
                          setAdminCfg({
                            ...adminCfg,
                            edge_storage: { ...adminCfg.edge_storage, region: e.target.value },
                          })
                        }
                      />
                    </div>

                    <div className="field">
                      <label htmlFor="edge-public">Public CDN / Pull Zone Base URL</label>
                      <input
                        id="edge-public"
                        type="text"
                        className="mono"
                        placeholder={
                          adminCfg.edge_storage.provider === "bunny"
                            ? "https://ourchive.b-cdn.net"
                            : "https://cdn.yourdomain.com"
                        }
                        value={adminCfg.edge_storage.public_base_url}
                        onChange={(e) =>
                          setAdminCfg({
                            ...adminCfg,
                            edge_storage: { ...adminCfg.edge_storage, public_base_url: e.target.value },
                          })
                        }
                      />
                    </div>

                    {adminCfg.edge_storage.provider === "s3" && (
                      <div className="field">
                        <label htmlFor="edge-access-key">Access Key ID</label>
                        <input
                          id="edge-access-key"
                          type="text"
                          className="mono"
                          value={adminCfg.edge_storage.access_key}
                          onChange={(e) =>
                            setAdminCfg({
                              ...adminCfg,
                              edge_storage: { ...adminCfg.edge_storage, access_key: e.target.value },
                            })
                          }
                        />
                      </div>
                    )}

                    <div className="field">
                      <label htmlFor="edge-secret-key">
                        {adminCfg.edge_storage.provider === "bunny" ? "Storage Zone Password (AccessKey)" : "Secret Access Key"}
                      </label>
                      <input
                        id="edge-secret-key"
                        type="password"
                        autoComplete="new-password"
                        placeholder={adminCfg.edge_storage.has_secret_key ? "•••••••• (saved; leave blank to keep)" : "Enter key"}
                        value={secretDraft}
                        onChange={(e) => setSecretDraft(e.target.value)}
                      />
                    </div>

                    <div className="field">
                      <label htmlFor="edge-max-mb">Max cached share size (MB)</label>
                      <input
                        id="edge-max-mb"
                        type="number"
                        min={0}
                        className="mono"
                        value={adminCfg.edge_storage.max_cache_mb}
                        onChange={(e) =>
                          setAdminCfg({
                            ...adminCfg,
                            edge_storage: {
                              ...adminCfg.edge_storage,
                              max_cache_mb: Math.max(0, Number(e.target.value) || 0),
                            },
                          })
                        }
                      />
                    </div>

                    <div className="field">
                      <label htmlFor="edge-ttl">Cache TTL (hours)</label>
                      <input
                        id="edge-ttl"
                        type="number"
                        min={0}
                        className="mono"
                        value={adminCfg.edge_storage.ttl_hours}
                        onChange={(e) =>
                          setAdminCfg({
                            ...adminCfg,
                            edge_storage: {
                              ...adminCfg.edge_storage,
                              ttl_hours: Math.max(0, Number(e.target.value) || 0),
                            },
                          })
                        }
                      />
                    </div>
                  </div>
                </>
              )}

              <div className="modal-actions">
                {adminCfg.edge_storage.enabled && adminCfg.edge_storage.provider !== "none" && (
                  <button type="button" className="btn" disabled={testing} onClick={handleTestEdge}>
                    {testing ? "Testing…" : "Test connection"}
                  </button>
                )}
                <button type="submit" className="btn btn-primary" disabled={saving}>
                  {saving ? "Saving…" : "Save settings"}
                </button>
              </div>
            </div>
          </section>
        </form>
      )}
    </div>
  );
}

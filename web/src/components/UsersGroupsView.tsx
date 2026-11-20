/**
 * UsersGroupsView — Governance dashboard with Members & recursive Invite Lineage Tree,
 * Invite Key minting/revocation, Groups with Host-Admin-governed download/preview audit toggles,
 * and a filterable Audit Log viewer.
 */
import { FormEvent, useCallback, useEffect, useMemo, useState } from "react";
import {
  Check,
  Copy,
  GitBranch,
  KeyRound,
  Plus,
  RefreshCw,
  ScrollText,
  ShieldAlert,
  Trash2,
  UserPlus,
  Users,
} from "lucide-react";
import { api, AuditEntry, formatDate, Group, InviteKey, PublicNetworkConfig, User } from "../api";
import { ModalDialog } from "./ModalDialog";

interface UsersGroupsViewProps {
  currentUser: User;
  netConfig: PublicNetworkConfig | null;
}

type PeopleTab = "members" | "invites" | "groups" | "audit";

export function UsersGroupsView({ currentUser, netConfig }: UsersGroupsViewProps) {
  const isHostAdmin = currentUser.role === "host_admin";
  const isGroupAdmin = currentUser.role === "group_admin";
  const canInvite =
    isHostAdmin ||
    currentUser.can_invite ||
    (isGroupAdmin && Boolean(netConfig?.allow_group_admin_invites));

  const [subTab, setSubTab] = useState<PeopleTab>(isHostAdmin || isGroupAdmin ? "members" : "invites");

  const [users, setUsers] = useState<User[]>([]);
  const [groups, setGroups] = useState<Group[]>([]);
  const [invites, setInvites] = useState<InviteKey[]>([]);
  const [auditEntries, setAuditEntries] = useState<AuditEntry[]>([]);
  const [auditTotal, setAuditTotal] = useState(0);
  const [auditCategory, setAuditCategory] = useState<"all" | "core" | "download" | "preview">("all");
  const [auditActor, setAuditActor] = useState("");
  const [auditGroup, setAuditGroup] = useState("");
  const [auditSearch, setAuditSearch] = useState("");
  const [auditLoading, setAuditLoading] = useState(false);

  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const [userModalOpen, setUserModalOpen] = useState(false);
  const [newUser, setNewUser] = useState({ email: "", username: "", display_name: "", password: "", role: "member" });

  const [inviteModalOpen, setInviteModalOpen] = useState(false);
  const [newInvite, setNewInvite] = useState({
    role: "member",
    group_id: "",
    max_uses: 1,
    expires_in_hours: 72,
    note: "",
  });
  const [mintedInvite, setMintedInvite] = useState<InviteKey | null>(null);
  const [copiedId, setCopiedId] = useState<string | null>(null);

  const [groupModalOpen, setGroupModalOpen] = useState(false);
  const [newGroup, setNewGroup] = useState({ name: "", description: "" });
  const [memberPick, setMemberPick] = useState<Record<string, string>>({});

  const refreshCore = useCallback(async () => {
    setError(null);
    try {
      const tasks: Promise<void>[] = [];
      if (isHostAdmin || isGroupAdmin) {
        tasks.push(api.listUsers().then((r) => setUsers(r.users)));
        tasks.push(api.listGroups().then((r) => setGroups(r.groups)));
      }
      if (canInvite) {
        tasks.push(api.listInvites().then((r) => setInvites(r.invites)));
      }
      await Promise.all(tasks);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to load people data");
    }
  }, [isHostAdmin, isGroupAdmin, canInvite]);

  const refreshAudit = useCallback(async () => {
    if (!isHostAdmin && !isGroupAdmin) return;
    if (!isHostAdmin && !auditGroup) {
      setAuditEntries([]);
      setAuditTotal(0);
      return;
    }
    setAuditLoading(true);
    setError(null);
    try {
      const res = await api.listAuditLogs({
        category: auditCategory,
        actor_id: auditActor || undefined,
        group_id: auditGroup || undefined,
        search: auditSearch.trim() || undefined,
        limit: 100,
      });
      setAuditEntries(res.entries);
      setAuditTotal(res.total);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to load audit log");
    } finally {
      setAuditLoading(false);
    }
  }, [isHostAdmin, isGroupAdmin, auditCategory, auditActor, auditGroup, auditSearch]);

  useEffect(() => {
    void refreshCore();
  }, [refreshCore]);

  useEffect(() => {
    if (subTab === "audit") {
      void refreshAudit();
    }
  }, [subTab, refreshAudit]);

  // Build recursive invite tree for Host Admin
  const inviteChildren = useMemo(() => {
    const map = new Map<string, User[]>();
    for (const u of users) {
      const parent = u.invited_by || "__root__";
      const list = map.get(parent) ?? [];
      list.push(u);
      map.set(parent, list);
    }
    return map;
  }, [users]);

  function copyText(id: string, text: string) {
    navigator.clipboard?.writeText(text).then(() => {
      setCopiedId(id);
      window.setTimeout(() => setCopiedId((prev) => (prev === id ? null : prev)), 1800);
    });
  }

  async function handleCreateUser(e: FormEvent) {
    e.preventDefault();
    setError(null);
    try {
      await api.createUser({
        email: newUser.email.trim() || undefined,
        username: newUser.username.trim() || undefined,
        display_name: newUser.display_name,
        password: newUser.password,
        role: newUser.role,
      });
      setNewUser({ email: "", username: "", display_name: "", password: "", role: "member" });
      setUserModalOpen(false);
      await refreshCore();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to create user");
    }
  }

  async function handleDeleteUser(u: User) {
    if (!window.confirm(`Delete account for ${u.display_name}?`)) return;
    setError(null);
    try {
      await api.deleteUser(u.id);
      await refreshCore();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to delete user");
    }
  }

  async function handleToggleTrustedInviter(u: User, nextCanInvite: boolean, nextQuota: number) {
    setError(null);
    try {
      await api.updateUserInvitePerms(u.id, { can_invite: nextCanInvite, invite_quota: nextQuota });
      setNotice(`Updated invite permissions for ${u.display_name}.`);
      await refreshCore();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to update invite permissions");
    }
  }

  async function handleCreateInvite(e: FormEvent) {
    e.preventDefault();
    setError(null);
    try {
      const res = await api.createInvite({
        role: isHostAdmin ? newInvite.role : "member",
        group_id: newInvite.group_id || undefined,
        max_uses: Number(newInvite.max_uses) || 1,
        expires_in_hours: Number(newInvite.expires_in_hours) || 0,
        note: newInvite.note,
      });
      setMintedInvite(res.invite);
      setInviteModalOpen(false);
      setNewInvite({ role: "member", group_id: "", max_uses: 1, expires_in_hours: 72, note: "" });
      await refreshCore();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to create invite key");
    }
  }

  async function handleRevokeInvite(k: InviteKey) {
    if (!window.confirm(`Revoke invite key ${k.code_prefix}…?`)) return;
    setError(null);
    try {
      await api.revokeInvite(k.id);
      await refreshCore();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to revoke invite key");
    }
  }

  async function handleCreateGroup(e: FormEvent) {
    e.preventDefault();
    setError(null);
    try {
      await api.createGroup(newGroup);
      setNewGroup({ name: "", description: "" });
      setGroupModalOpen(false);
      await refreshCore();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to create group");
    }
  }

  async function handleToggleGroupAudit(g: Group, nextDownloads: boolean, nextPreviews: boolean) {
    setError(null);
    try {
      await api.updateGroupAudit(g.id, { audit_downloads: nextDownloads, audit_previews: nextPreviews });
      setNotice(`Updated audit settings for ${g.name}.`);
      await refreshCore();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to update group audit settings");
    }
  }

  async function handleAddMember(groupId: string) {
    const userId = memberPick[groupId];
    if (!userId) return;
    await api.addGroupMember(groupId, { user_id: userId, role: "member" });
    setMemberPick((prev) => ({ ...prev, [groupId]: "" }));
    await refreshCore();
  }

  async function handleRemoveMember(groupId: string, userId: string) {
    await api.removeGroupMember(groupId, userId);
    await refreshCore();
  }

  function renderTreeBranch(parentId: string, depth: number) {
    const children = inviteChildren.get(parentId) ?? [];
    if (children.length === 0) return null;
    return (
      <div style={{ marginLeft: depth === 0 ? 0 : 20, borderLeft: depth === 0 ? "none" : "1px solid var(--border)", paddingLeft: depth === 0 ? 0 : 12 }}>
        {children.map((u) => {
          const grandChildren = inviteChildren.get(u.id) ?? [];
          return (
            <div key={u.id} style={{ padding: "6px 0" }}>
              <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap", fontSize: 13 }}>
                <strong>{u.display_name}</strong>
                {u.username && <span className="mono node-sub">@{u.username}</span>}
                {u.email && <span className="mono node-sub">{u.email}</span>}
                <span className="tag-chip mono">{u.role}</span>
                {u.invite_key_prefix && (
                  <span className="tag-chip mono" title="Redeemed invite key">
                    key {u.invite_key_prefix}…
                  </span>
                )}
                {grandChildren.length > 0 && (
                  <span className="node-sub">· invited {grandChildren.length}</span>
                )}
              </div>
              {renderTreeBranch(u.id, depth + 1)}
            </div>
          );
        })}
      </div>
    );
  }

  const globalDownloadsOn = Boolean(netConfig?.audit_downloads_enabled);
  const globalPreviewsOn = Boolean(netConfig?.audit_previews_enabled);

  return (
    <div className="section-stack">
      {/* Sub-navigation */}
      <div className="segmented" role="tablist" aria-label="People and governance sections" style={{ alignSelf: "flex-start" }}>
        {(isHostAdmin || isGroupAdmin) && (
          <button
            type="button"
            role="tab"
            className="seg-btn"
            aria-pressed={subTab === "members"}
            onClick={() => setSubTab("members")}
          >
            Members &amp; Tree
          </button>
        )}
        {canInvite && (
          <button
            type="button"
            role="tab"
            className="seg-btn"
            aria-pressed={subTab === "invites"}
            onClick={() => setSubTab("invites")}
          >
            Invites ({invites.length})
          </button>
        )}
        {(isHostAdmin || isGroupAdmin) && (
          <button
            type="button"
            role="tab"
            className="seg-btn"
            aria-pressed={subTab === "groups"}
            onClick={() => setSubTab("groups")}
          >
            Groups ({groups.length})
          </button>
        )}
        {(isHostAdmin || isGroupAdmin) && (
          <button
            type="button"
            role="tab"
            className="seg-btn"
            aria-pressed={subTab === "audit"}
            onClick={() => setSubTab("audit")}
          >
            Audit Log
          </button>
        )}
      </div>

      {notice && (
        <div className="status-banner" role="status">
          <Check size={14} style={{ verticalAlign: "middle", marginRight: 6 }} />
          {notice}
        </div>
      )}
      {error && (
        <div className="alert-error" role="alert">
          {error}
        </div>
      )}

      {/* Newly minted invite key banner */}
      {mintedInvite && mintedInvite.code && (
        <section className="card" style={{ borderColor: "var(--accent)" }}>
          <div className="section-head">
            <div>
              <h3 className="section-title">
                <KeyRound size={15} style={{ verticalAlign: "text-bottom", marginRight: 6 }} />
                Invite key created (shown once)
              </h3>
              <p className="page-sub">Share this link or key with the person you are inviting.</p>
            </div>
            <button type="button" className="btn" onClick={() => setMintedInvite(null)}>
              Dismiss
            </button>
          </div>
          <div style={{ display: "flex", gap: 8, flexWrap: "wrap", alignItems: "center", marginTop: 8 }}>
            <code className="tag-chip mono" style={{ fontSize: 13, padding: "6px 10px" }}>
              {mintedInvite.code}
            </code>
            <button
              type="button"
              className="btn btn-primary"
              onClick={() =>
                copyText("minted-url", `${window.location.origin}/?invite=${encodeURIComponent(mintedInvite.code!)}`)
              }
            >
              {copiedId === "minted-url" ? <Check size={14} /> : <Copy size={14} />}
              <span>{copiedId === "minted-url" ? "Copied link" : "Copy invite link"}</span>
            </button>
            <button
              type="button"
              className="btn"
              onClick={() => copyText("minted-code", mintedInvite.code!)}
            >
              {copiedId === "minted-code" ? <Check size={14} /> : <Copy size={14} />}
              <span>{copiedId === "minted-code" ? "Copied key" : "Copy key"}</span>
            </button>
          </div>
        </section>
      )}

      {/* TAB 1: MEMBERS & INVITE TREE */}
      {subTab === "members" && (isHostAdmin || isGroupAdmin) && (
        <>
          <section className="card">
            <div className="section-head">
              <div>
                <h2 className="section-title">Members</h2>
                <p className="page-sub">
                  Accounts with access to this Ourchive hub, their invite lineage, and trusted inviter permissions.
                </p>
              </div>
              <div style={{ display: "flex", gap: 8 }}>
                {canInvite && (
                  <button type="button" className="btn btn-primary" onClick={() => setInviteModalOpen(true)}>
                    <KeyRound size={14} strokeWidth={1.6} />
                    <span>New invite key</span>
                  </button>
                )}
                {isHostAdmin && (
                  <button type="button" className="btn" onClick={() => setUserModalOpen(true)}>
                    <UserPlus size={14} strokeWidth={1.6} />
                    <span>Add directly</span>
                  </button>
                )}
              </div>
            </div>

            <div className="table-wrap">
              <table className="data-table">
                <thead>
                  <tr>
                    <th>Name</th>
                    <th>Identity</th>
                    <th>Role</th>
                    <th>Invited by</th>
                    {isHostAdmin && <th>Trusted inviter</th>}
                    <th className="num-col">Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {users.map((u) => (
                    <tr key={u.id}>
                      <td>{u.display_name}</td>
                      <td className="mono">
                        {u.username ? `@${u.username}` : ""}
                        {u.username && u.email ? " · " : ""}
                        {u.email || (!u.username ? "—" : "")}
                      </td>
                      <td>
                        <span className="tag-chip mono">{u.role}</span>
                      </td>
                      <td>
                        {u.invited_by_name ? (
                          <span>
                            {u.invited_by_name}
                            {u.invite_key_prefix && (
                              <span className="node-sub mono" style={{ marginLeft: 6 }}>
                                ({u.invite_key_prefix}…)
                              </span>
                            )}
                          </span>
                        ) : (
                          <span className="node-sub">Root</span>
                        )}
                      </td>
                      {isHostAdmin && (
                        <td>
                          {u.role === "host_admin" ? (
                            <span className="node-sub">Always (Host Admin)</span>
                          ) : (
                            <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                              <input
                                type="checkbox"
                                aria-label={`Allow ${u.display_name} to create invites`}
                                checked={u.can_invite}
                                onChange={(e) => handleToggleTrustedInviter(u, e.target.checked, u.invite_quota)}
                                {...({ switch: "" } as Record<string, string>)}
                              />
                              {u.can_invite && (
                                <input
                                  type="number"
                                  min={0}
                                  title="Invite quota (0 = unlimited)"
                                  aria-label={`Invite quota for ${u.display_name}`}
                                  className="mono"
                                  style={{ width: 64, height: 28, padding: "2px 6px" }}
                                  value={u.invite_quota}
                                  onChange={(e) =>
                                    handleToggleTrustedInviter(u, true, Math.max(0, Number(e.target.value) || 0))
                                  }
                                />
                              )}
                            </div>
                          )}
                        </td>
                      )}
                      <td className="num-col">
                        {isHostAdmin && u.id !== currentUser.id && (
                          <button
                            type="button"
                            className="icon-btn danger"
                            aria-label={`Delete ${u.display_name}`}
                            onClick={() => handleDeleteUser(u)}
                          >
                            <Trash2 size={14} strokeWidth={1.5} />
                          </button>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </section>

          {isHostAdmin && (
            <section className="card">
              <div className="section-head">
                <div>
                  <h2 className="section-title">
                    <GitBranch size={15} strokeWidth={1.6} style={{ verticalAlign: "text-bottom", marginRight: 6 }} />
                    Invite lineage tree
                  </h2>
                  <p className="page-sub">Every account traced back to the inviter and invite key that brought them in.</p>
                </div>
              </div>
              {renderTreeBranch("__root__", 0)}
            </section>
          )}
        </>
      )}

      {/* TAB 2: INVITE KEYS */}
      {subTab === "invites" && canInvite && (
        <section className="card">
          <div className="section-head">
            <div>
              <h2 className="section-title">Invite keys</h2>
              <p className="page-sub">
                {isHostAdmin
                  ? "All invite keys across the hub, who minted them, and every account that redeemed them."
                  : "Invite keys you have minted for friends."}
              </p>
            </div>
            <button type="button" className="btn btn-primary" onClick={() => setInviteModalOpen(true)}>
              <Plus size={14} strokeWidth={1.6} />
              <span>Generate invite key</span>
            </button>
          </div>

          {invites.length === 0 ? (
            <div className="mounts-empty">
              <KeyRound size={15} strokeWidth={1.5} style={{ verticalAlign: "middle", marginRight: 8 }} />
              No invite keys created yet.
            </div>
          ) : (
            <div className="table-wrap">
              <table className="data-table">
                <thead>
                  <tr>
                    <th>Key</th>
                    <th>Created by</th>
                    <th>Role / Group</th>
                    <th>Uses</th>
                    <th>Status</th>
                    <th>Redeemed by</th>
                    <th className="num-col">Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {invites.map((k) => {
                    const exhausted = k.max_uses > 0 && k.use_count >= k.max_uses;
                    const statusLabel = k.revoked
                      ? "Revoked"
                      : k.expired
                        ? "Expired"
                        : exhausted
                          ? "Used"
                          : "Active";
                    return (
                      <tr key={k.id}>
                        <td>
                          <div className="mono">{k.code_prefix}…</div>
                          {k.note && <div className="node-sub">{k.note}</div>}
                        </td>
                        <td>{k.creator_name || "—"}</td>
                        <td>
                          <span className="tag-chip mono">{k.role}</span>
                          {k.group_name && (
                            <span className="tag-chip" style={{ marginLeft: 4 }}>
                              {k.group_name}
                            </span>
                          )}
                        </td>
                        <td className="mono">
                          {k.use_count} / {k.max_uses}
                        </td>
                        <td>
                          <span className="tag-chip mono">{statusLabel}</span>
                          {k.expires_at && !k.revoked && (
                            <div className="node-sub">exp {formatDate(k.expires_at)}</div>
                          )}
                        </td>
                        <td>
                          {k.redeemed_by.length === 0 ? (
                            <span className="node-sub">None yet</span>
                          ) : (
                            <div className="member-chips">
                              {k.redeemed_by.map((ru) => (
                                <span key={ru.id} className="member-chip">
                                  {ru.display_name}
                                  {ru.username ? ` (@${ru.username})` : ""}
                                </span>
                              ))}
                            </div>
                          )}
                        </td>
                        <td className="num-col">
                          {!k.revoked && !exhausted && !k.expired && (
                            <button
                              type="button"
                              className="icon-btn danger"
                              title="Revoke invite key"
                              aria-label={`Revoke ${k.code_prefix}`}
                              onClick={() => handleRevokeInvite(k)}
                            >
                              <Trash2 size={14} strokeWidth={1.5} />
                            </button>
                          )}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
        </section>
      )}

      {/* TAB 3: GROUPS & GROUP AUDIT OVERRIDES */}
      {subTab === "groups" && (isHostAdmin || isGroupAdmin) && (
        <section className="card">
          <div className="section-head">
            <div>
              <h2 className="section-title">Groups</h2>
              <p className="page-sub">
                Assign folder permissions, shared GPU access, and optional group-level download/preview audit toggles.
              </p>
            </div>
            {isHostAdmin && (
              <button type="button" className="btn" onClick={() => setGroupModalOpen(true)}>
                <Plus size={15} strokeWidth={1.6} />
                <span>New group</span>
              </button>
            )}
          </div>

          {groups.length === 0 ? (
            <div className="mounts-empty">
              <Users size={16} strokeWidth={1.5} style={{ verticalAlign: "middle", marginRight: 8 }} />
              No groups created yet.
            </div>
          ) : (
            <div className="group-list">
              {groups.map((g) => {
                const memberIds = new Set(g.members.map((m) => m.user_id));
                const available = users.filter((u) => !memberIds.has(u.id));
                const canEditAudit =
                  isHostAdmin ||
                  g.members.some((m) => m.user_id === currentUser.id && (m.role === "admin" || isGroupAdmin));

                return (
                  <div key={g.id} className="group-item">
                    <div className="group-head">
                      <div>
                        <strong>{g.name}</strong>
                        {g.description && <div className="node-sub">{g.description}</div>}
                      </div>
                      {isHostAdmin && (
                        <button
                          type="button"
                          className="icon-btn danger"
                          aria-label={`Delete group ${g.name}`}
                          onClick={async () => {
                            await api.deleteGroup(g.id);
                            await refreshCore();
                          }}
                        >
                          <Trash2 size={14} strokeWidth={1.5} />
                        </button>
                      )}
                    </div>

                    <div className="member-chips">
                      {g.members.length === 0 ? (
                        <span className="node-sub">No members yet</span>
                      ) : (
                        g.members.map((m) => (
                          <span key={m.user_id} className="member-chip">
                            {m.display_name}
                            <button
                              type="button"
                              aria-label={`Remove ${m.display_name} from ${g.name}`}
                              onClick={() => handleRemoveMember(g.id, m.user_id)}
                            >
                              ×
                            </button>
                          </span>
                        ))
                      )}
                    </div>

                    {available.length > 0 && (
                      <div className="add-member-row">
                        <select
                          value={memberPick[g.id] ?? ""}
                          aria-label={`Select user to add to ${g.name}`}
                          onChange={(e) => setMemberPick((prev) => ({ ...prev, [g.id]: e.target.value }))}
                        >
                          <option value="">Add member to {g.name}…</option>
                          {available.map((u) => (
                            <option key={u.id} value={u.id}>
                              {u.display_name} ({u.username ? `@${u.username}` : u.email})
                            </option>
                          ))}
                        </select>
                        <button type="button" className="btn" onClick={() => handleAddMember(g.id)}>
                          Add
                        </button>
                      </div>
                    )}

                    {canEditAudit && (
                      <div className="node-controls" style={{ marginTop: 10, paddingTop: 10, borderTop: "1px solid var(--border)" }}>
                        <label className="switch-row" style={{ opacity: globalDownloadsOn ? 1 : 0.55 }}>
                          <span>
                            <strong>Audit file downloads in {g.name}</strong>
                            <small>
                              {globalDownloadsOn
                                ? "Record downloads for this group"
                                : "Disabled globally by Host Admin (cannot be enabled here)"}
                            </small>
                          </span>
                          <input
                            type="checkbox"
                            disabled={!globalDownloadsOn}
                            checked={globalDownloadsOn && g.audit_downloads}
                            onChange={(e) => handleToggleGroupAudit(g, e.target.checked, g.audit_previews)}
                            {...({ switch: "" } as Record<string, string>)}
                          />
                        </label>

                        <label className="switch-row" style={{ opacity: globalPreviewsOn ? 1 : 0.55 }}>
                          <span>
                            <strong>Audit file previews in {g.name}</strong>
                            <small>
                              {globalPreviewsOn
                                ? "Record previews for this group"
                                : "Disabled globally by Host Admin (cannot be enabled here)"}
                            </small>
                          </span>
                          <input
                            type="checkbox"
                            disabled={!globalPreviewsOn}
                            checked={globalPreviewsOn && g.audit_previews}
                            onChange={(e) => handleToggleGroupAudit(g, g.audit_downloads, e.target.checked)}
                            {...({ switch: "" } as Record<string, string>)}
                          />
                        </label>
                      </div>
                    )}
                  </div>
                );
              })}
            </div>
          )}
        </section>
      )}

      {/* TAB 4: AUDIT LOG */}
      {subTab === "audit" && (isHostAdmin || isGroupAdmin) && (
        <section className="card">
          <div className="section-head">
            <div>
              <h2 className="section-title">
                <ScrollText size={16} strokeWidth={1.6} style={{ verticalAlign: "text-bottom", marginRight: 8 }} />
                Audit log
              </h2>
              <p className="page-sub">
                Differentiated activity trail ({auditTotal} matching events). Configure active tiers in Settings.
              </p>
            </div>
            <button type="button" className="btn" onClick={() => void refreshAudit()} disabled={auditLoading}>
              <RefreshCw size={14} strokeWidth={1.6} />
              <span>Refresh</span>
            </button>
          </div>

          <div style={{ display: "flex", gap: 8, flexWrap: "wrap", marginBottom: 14 }}>
            <select
              aria-label="Filter audit category"
              value={auditCategory}
              onChange={(e) => setAuditCategory(e.target.value as "all" | "core" | "download" | "preview")}
            >
              <option value="all">All categories</option>
              <option value="core">Core activity (auth, invites, uploads, admin)</option>
              <option value="download">File downloads</option>
              <option value="preview">File previews</option>
            </select>

            {isHostAdmin && (
              <select aria-label="Filter by user" value={auditActor} onChange={(e) => setAuditActor(e.target.value)}>
                <option value="">All users</option>
                {users.map((u) => (
                  <option key={u.id} value={u.id}>
                    {u.display_name} ({u.username ? `@${u.username}` : u.email})
                  </option>
                ))}
              </select>
            )}

            <select aria-label="Filter by group" value={auditGroup} onChange={(e) => setAuditGroup(e.target.value)}>
              <option value="">{isHostAdmin ? "All groups" : "Select a group you admin…"}</option>
              {groups.map((g) => (
                <option key={g.id} value={g.id}>
                  {g.name}
                </option>
              ))}
            </select>

            <input
              type="search"
              placeholder="Search action, target, IP…"
              value={auditSearch}
              onChange={(e) => setAuditSearch(e.target.value)}
              style={{ flex: "1 1 180px" }}
            />
          </div>

          {!isHostAdmin && !auditGroup ? (
            <div className="mounts-empty">
              <ShieldAlert size={15} style={{ verticalAlign: "middle", marginRight: 8 }} />
              Select one of your groups above to inspect its audit trail.
            </div>
          ) : auditEntries.length === 0 ? (
            <div className="mounts-empty">No audit events recorded for this filter.</div>
          ) : (
            <div className="table-wrap">
              <table className="data-table">
                <thead>
                  <tr>
                    <th>Time</th>
                    <th>Tier</th>
                    <th>Actor</th>
                    <th>Action</th>
                    <th>Target / Details</th>
                    <th>IP</th>
                  </tr>
                </thead>
                <tbody>
                  {auditEntries.map((ev) => (
                    <tr key={ev.id}>
                      <td className="mono" style={{ whiteSpace: "nowrap", fontSize: 12 }}>
                        {new Date(ev.created_at).toLocaleString()}
                      </td>
                      <td>
                        <span
                          className="tag-chip mono"
                          style={
                            ev.category === "download"
                              ? { borderColor: "var(--ok)", color: "var(--ok)" }
                              : ev.category === "preview"
                                ? { borderColor: "var(--io-up)", color: "var(--io-up)" }
                                : undefined
                          }
                        >
                          {ev.category}
                        </span>
                      </td>
                      <td>{ev.actor_label || "—"}</td>
                      <td className="mono">{ev.action}</td>
                      <td className="mono" style={{ fontSize: 12, maxWidth: 280, overflow: "hidden", textOverflow: "ellipsis" }}>
                        {ev.target_id || (ev.metadata ? JSON.stringify(ev.metadata) : "—")}
                      </td>
                      <td className="mono" style={{ fontSize: 12 }}>
                        {ev.ip_address || "—"}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </section>
      )}

      {/* MODAL: GENERATE INVITE KEY */}
      <ModalDialog open={inviteModalOpen} title="Generate invite key" onClose={() => setInviteModalOpen(false)}>
        <form className="form-stack" onSubmit={handleCreateInvite}>
          {isHostAdmin && (
            <div className="field">
              <label htmlFor="inv-role">Assigned role</label>
              <select
                id="inv-role"
                value={newInvite.role}
                onChange={(e) => setNewInvite((p) => ({ ...p, role: e.target.value }))}
              >
                <option value="member">Member</option>
                <option value="group_admin">Group Admin</option>
                <option value="host_admin">Host Admin</option>
              </select>
            </div>
          )}

          {groups.length > 0 && (
            <div className="field">
              <label htmlFor="inv-group">Auto-add to group (optional)</label>
              <select
                id="inv-group"
                value={newInvite.group_id}
                onChange={(e) => setNewInvite((p) => ({ ...p, group_id: e.target.value }))}
              >
                <option value="">None</option>
                {groups.map((g) => (
                  <option key={g.id} value={g.id}>
                    {g.name}
                  </option>
                ))}
              </select>
            </div>
          )}

          <div className="settings-grid-2">
            <div className="field">
              <label htmlFor="inv-uses">Max uses</label>
              <input
                id="inv-uses"
                type="number"
                min={1}
                max={isHostAdmin ? 100 : 5}
                className="mono"
                value={newInvite.max_uses}
                onChange={(e) => setNewInvite((p) => ({ ...p, max_uses: Math.max(1, Number(e.target.value) || 1) }))}
              />
            </div>

            <div className="field">
              <label htmlFor="inv-exp">Expires in (hours, 0 = never)</label>
              <input
                id="inv-exp"
                type="number"
                min={0}
                className="mono"
                value={newInvite.expires_in_hours}
                onChange={(e) =>
                  setNewInvite((p) => ({ ...p, expires_in_hours: Math.max(0, Number(e.target.value) || 0) }))
                }
              />
            </div>
          </div>

          <div className="field">
            <label htmlFor="inv-note">Note (optional)</label>
            <input
              id="inv-note"
              type="text"
              placeholder="e.g. For Alex"
              value={newInvite.note}
              onChange={(e) => setNewInvite((p) => ({ ...p, note: e.target.value }))}
            />
          </div>

          <div className="modal-actions">
            <button type="button" className="btn" onClick={() => setInviteModalOpen(false)}>
              Cancel
            </button>
            <button type="submit" className="btn btn-primary">
              Create invite key
            </button>
          </div>
        </form>
      </ModalDialog>

      {/* MODAL: ADD MEMBER DIRECTLY (HOST ADMIN) */}
      <ModalDialog open={userModalOpen} title="Add member directly" onClose={() => setUserModalOpen(false)}>
        <form className="form-stack" onSubmit={handleCreateUser}>
          <div className="field">
            <label htmlFor="new-user-name">Display name</label>
            <input
              id="new-user-name"
              type="text"
              autoComplete="name"
              required
              value={newUser.display_name}
              onChange={(e) => setNewUser((p) => ({ ...p, display_name: e.target.value }))}
            />
          </div>
          <div className="field">
            <label htmlFor="new-user-username">Username</label>
            <input
              id="new-user-username"
              type="text"
              autoComplete="off"
              spellCheck={false}
              value={newUser.username}
              onChange={(e) => setNewUser((p) => ({ ...p, username: e.target.value }))}
              placeholder="Optional depending on hub policy"
            />
          </div>
          <div className="field">
            <label htmlFor="new-user-email">Email</label>
            <input
              id="new-user-email"
              type="email"
              autoComplete="off"
              spellCheck={false}
              value={newUser.email}
              onChange={(e) => setNewUser((p) => ({ ...p, email: e.target.value }))}
              placeholder="Optional depending on hub policy"
            />
          </div>
          <div className="field">
            <label htmlFor="new-user-pass">Temporary password</label>
            <span className="field-hint">At least 8 characters.</span>
            <input
              id="new-user-pass"
              type="password"
              autoComplete="new-password"
              required
              minLength={8}
              value={newUser.password}
              onChange={(e) => setNewUser((p) => ({ ...p, password: e.target.value }))}
            />
          </div>
          <div className="field">
            <label htmlFor="new-user-role">Role</label>
            <select
              id="new-user-role"
              value={newUser.role}
              onChange={(e) => setNewUser((p) => ({ ...p, role: e.target.value }))}
            >
              <option value="member">Member</option>
              <option value="group_admin">Group Admin</option>
              <option value="host_admin">Host Admin</option>
            </select>
          </div>
          <div className="modal-actions">
            <button type="button" className="btn" onClick={() => setUserModalOpen(false)}>
              Cancel
            </button>
            <button type="submit" className="btn btn-primary">
              Create account
            </button>
          </div>
        </form>
      </ModalDialog>

      {/* MODAL: CREATE GROUP */}
      <ModalDialog open={groupModalOpen} title="Create group" onClose={() => setGroupModalOpen(false)}>
        <form className="form-stack" onSubmit={handleCreateGroup}>
          <div className="field">
            <label htmlFor="new-group-name">Group name</label>
            <input
              id="new-group-name"
              type="text"
              required
              value={newGroup.name}
              onChange={(e) => setNewGroup((p) => ({ ...p, name: e.target.value }))}
              placeholder="e.g. Friends"
            />
          </div>
          <div className="field">
            <label htmlFor="new-group-desc">Description</label>
            <input
              id="new-group-desc"
              type="text"
              value={newGroup.description}
              onChange={(e) => setNewGroup((p) => ({ ...p, description: e.target.value }))}
              placeholder="Optional note"
            />
          </div>
          <div className="modal-actions">
            <button type="button" className="btn" onClick={() => setGroupModalOpen(false)}>
              Cancel
            </button>
            <button type="submit" className="btn btn-primary">
              Create group
            </button>
          </div>
        </form>
      </ModalDialog>
    </div>
  );
}

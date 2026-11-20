export interface HealthPayload {
  status: string;
  version: string;
  share_host: string;
  time: string;
}

export type AuthIdentityMode = "email_or_username" | "email_only" | "username_only" | "both";

export interface User {
  id: string;
  email: string;
  username?: string;
  display_name: string;
  role: "host_admin" | "group_admin" | "member";
  invited_by?: string;
  invited_by_name?: string;
  invite_key_id?: string;
  invite_key_prefix?: string;
  can_invite: boolean;
  invite_quota: number;
  created_at: string;
}

export interface AuthStatus {
  initialized: boolean;
  auth_identity_mode?: AuthIdentityMode;
  user: User | null;
}

export interface InvitedUser {
  id: string;
  display_name: string;
  username?: string;
  email?: string;
  created_at: string;
}

export interface InviteKey {
  id: string;
  code_prefix: string;
  code?: string;
  created_by: string;
  creator_name: string;
  role: string;
  group_id?: string;
  group_name?: string;
  max_uses: number;
  use_count: number;
  note: string;
  expires_at?: string;
  revoked_at?: string;
  expired: boolean;
  revoked: boolean;
  created_at: string;
  redeemed_by: InvitedUser[];
}

export interface AuditEntry {
  id: string;
  created_at: string;
  category: "core" | "download" | "preview";
  action: string;
  actor_id?: string;
  actor_label: string;
  group_id?: string;
  target_type?: string;
  target_id?: string;
  metadata?: Record<string, unknown>;
  ip_address?: string;
}

export interface GroupMember {
  user_id: string;
  email: string;
  username?: string;
  display_name: string;
  role: "admin" | "member";
}

export interface Group {
  id: string;
  name: string;
  description: string;
  audit_downloads: boolean;
  audit_previews: boolean;
  created_at: string;
  members: GroupMember[];
}

export interface FolderMount {
  id: string;
  name: string;
  local_path: string;
  kind?: "folder" | "file";
  enabled: boolean;
  allow_sync?: boolean;
  total_bytes?: number;
  rejected?: boolean;
  reject_msg?: string;
}

export interface SyncSubscription {
  id: string;
  source_node_id: string;
  source_node_name: string;
  source_mount_id: string;
  source_mount_name: string;
  contributor_ok: boolean;
  target_node_id: string;
  target_node_name: string;
  target_path: string;
  enabled: boolean;
  created_at: string;
}

export interface ComputeCapabilities {
  enabled: boolean;
  ffmpeg_path?: string;
  cpu_model?: string;
  gpu_model?: string;
  gpu_models?: string[];
  hw_encoders?: string[];
  cpu_cores: number;
  max_concurrent: number;
  active_jobs: number;
}

export interface TrafficStats {
  upload_bps: number;
  download_bps: number;
  upload_cap_mbps: number;
}

export interface NodeView {
  id: string;
  owner_id: string;
  owner_name: string;
  is_owner: boolean;
  display_name: string;
  os: string;
  arch: string;
  status: "online" | "offline" | "paused";
  paused: boolean;
  upload_cap_mbps: number;
  allow_group_compute: boolean;
  compute: ComputeCapabilities;
  traffic: TrafficStats;
  mounts: FolderMount[];
  last_seen_at?: string;
  created_at: string;
}

export interface EnrollTokenResponse {
  token: string;
  expires_at: string;
  command: string;
}

export interface FileEntry {
  name: string;
  path: string;
  is_dir: boolean;
  size: number;
  mod_time: string;
  mime_type: string;
  tags?: string[];
}

export interface ListResponse {
  path: string;
  entries: FileEntry[];
}

export interface TagSummary {
  id: string;
  slug: string;
  description: string;
  item_count: number;
  created_at: string;
}

export interface TreeItem {
  id: string;
  tag_id: string;
  tag_slug: string;
  node_id: string;
  node_name: string;
  node_status: "online" | "offline" | "paused";
  traffic: TrafficStats;
  mount_id: string;
  mount_name: string;
  rel_path: string;
  is_dir: boolean;
  created_at: string;
}

export interface ShareLink {
  slug: string;
  node_id: string;
  node_name: string;
  node_status: "online" | "offline" | "paused";
  mount_id: string;
  mount_name: string;
  rel_path: string;
  is_dir: boolean;
  title: string;
  mime_type: string;
  size_bytes: number;
  has_password: boolean;
  max_views?: number;
  view_count: number;
  expires_at?: string;
  expired: boolean;
  created_by: string;
  created_at: string;
  url: string;
}

export interface EdgeStorageConfig {
  enabled: boolean;
  provider: "none" | "s3" | "bunny";
  endpoint: string;
  bucket: string;
  region: string;
  public_base_url: string;
  access_key: string;
  secret_key?: string;
  has_secret_key: boolean;
  max_cache_mb: number;
  ttl_hours: number;
}

export interface HubSettings {
  p2p_enabled: boolean;
  stun_servers: string[];
  allow_relay_fallback: boolean;
  allow_share_relay: boolean;
  max_relay_file_mb: number;
  relay_cap_mbps: number;
  edge_storage: EdgeStorageConfig;
  auth_identity_mode: AuthIdentityMode;
  allow_group_admin_invites: boolean;
  audit_core_enabled: boolean;
  audit_downloads_enabled: boolean;
  audit_previews_enabled: boolean;
  dev_console_enabled: boolean;
}

export interface PublicNetworkConfig {
  p2p_enabled: boolean;
  stun_servers: string[];
  allow_relay_fallback: boolean;
  allow_share_relay: boolean;
  max_relay_file_mb: number;
  edge_storage_active: boolean;
  edge_provider: string;
  auth_identity_mode: AuthIdentityMode;
  allow_group_admin_invites: boolean;
  audit_core_enabled: boolean;
  audit_downloads_enabled: boolean;
  audit_previews_enabled: boolean;
  dev_console_enabled: boolean;
}

export type AnnouncementKind = "broadcast" | "notice" | "warning" | "error";

export interface Announcement {
  id: string;
  title: string;
  body: string;
  kind: AnnouncementKind;
  dismissible: boolean;
  expires_at?: string;
  created_by: string;
  creator_name: string;
  created_at: string;
}

export interface ComputeJob {
  id: string;
  target_node_id: string;
  node_name: string;
  output_format: string;
  preset: string;
  status: string;
  error_message?: string;
  created_at: string;
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    credentials: "same-origin",
    headers: {
      "Content-Type": "application/json",
      ...(init?.headers ?? {}),
    },
    ...init,
  });
  const text = await res.text();
  const data = text ? JSON.parse(text) : {};
  if (!res.ok) {
    throw new Error(data.error || `Request failed (${res.status})`);
  }
  return data as T;
}

export const api = {
  health: () => request<HealthPayload>("/api/health"),
  authStatus: () => request<AuthStatus>("/api/auth/status"),
  setupAdmin: (body: { email: string; username?: string; display_name: string; password: string }) =>
    request<{ user: User }>("/api/auth/setup", { method: "POST", body: JSON.stringify(body) }),
  login: (body: { identifier: string; password: string }) =>
    request<{ user: User }>("/api/auth/login", { method: "POST", body: JSON.stringify(body) }),
  registerWithInvite: (body: {
    invite_code: string;
    email?: string;
    username?: string;
    display_name: string;
    password: string;
  }) => request<{ user: User }>("/api/auth/register", { method: "POST", body: JSON.stringify(body) }),
  logout: () => request<{ ok: boolean }>("/api/auth/logout", { method: "POST" }),

  listUsers: () => request<{ users: User[] }>("/api/users"),
  createUser: (body: { email?: string; username?: string; display_name: string; password: string; role: string }) =>
    request<{ user: User }>("/api/users", { method: "POST", body: JSON.stringify(body) }),
  updateUserInvitePerms: (id: string, body: { can_invite: boolean; invite_quota: number }) =>
    request<{ ok: boolean }>(`/api/users/${id}/invite-permissions`, { method: "PATCH", body: JSON.stringify(body) }),
  deleteUser: (id: string) => request<{ ok: boolean }>(`/api/users/${id}`, { method: "DELETE" }),

  listInvites: () => request<{ invites: InviteKey[] }>("/api/invites"),
  createInvite: (body: {
    role?: string;
    group_id?: string;
    max_uses?: number;
    expires_in_hours?: number;
    note?: string;
  }) => request<{ invite: InviteKey }>("/api/invites", { method: "POST", body: JSON.stringify(body) }),
  revokeInvite: (id: string) => request<{ ok: boolean }>(`/api/invites/${id}`, { method: "DELETE" }),

  listGroups: () => request<{ groups: Group[] }>("/api/groups"),
  createGroup: (body: { name: string; description: string }) =>
    request<{ group: Group }>("/api/groups", { method: "POST", body: JSON.stringify(body) }),
  updateGroupAudit: (groupId: string, body: { audit_downloads: boolean; audit_previews: boolean }) =>
    request<{ ok: boolean }>(`/api/groups/${groupId}/audit`, { method: "PATCH", body: JSON.stringify(body) }),
  deleteGroup: (id: string) => request<{ ok: boolean }>(`/api/groups/${id}`, { method: "DELETE" }),
  addGroupMember: (groupId: string, body: { user_id: string; role: string }) =>
    request<{ ok: boolean }>(`/api/groups/${groupId}/members`, { method: "POST", body: JSON.stringify(body) }),
  removeGroupMember: (groupId: string, userId: string) =>
    request<{ ok: boolean }>(`/api/groups/${groupId}/members/${userId}`, { method: "DELETE" }),

  listAuditLogs: (params: {
    category?: string;
    actor_id?: string;
    group_id?: string;
    search?: string;
    limit?: number;
    offset?: number;
  }) => {
    const q = new URLSearchParams();
    if (params.category && params.category !== "all") q.set("category", params.category);
    if (params.actor_id) q.set("actor_id", params.actor_id);
    if (params.group_id) q.set("group_id", params.group_id);
    if (params.search) q.set("search", params.search);
    if (params.limit) q.set("limit", String(params.limit));
    if (params.offset) q.set("offset", String(params.offset));
    return request<{ entries: AuditEntry[]; total: number }>(`/api/audit-logs?${q.toString()}`);
  },

  listNodes: () => request<{ nodes: NodeView[] }>("/api/nodes"),
  createEnrollToken: (displayName: string) =>
    request<EnrollTokenResponse>("/api/nodes/enroll-token", {
      method: "POST",
      body: JSON.stringify({ display_name: displayName }),
    }),
  updateNodeConfig: (
    nodeId: string,
    body: { display_name?: string; paused?: boolean; upload_cap_mbps?: number; allow_group_compute?: boolean },
  ) => request<{ ok: boolean }>(`/api/nodes/${nodeId}/config`, { method: "PATCH", body: JSON.stringify(body) }),
  deleteNode: (nodeId: string) => request<{ ok: boolean }>(`/api/nodes/${nodeId}`, { method: "DELETE" }),

  addMount: (nodeId: string, body: { name: string; local_path: string; kind?: "folder" | "file" }) =>
    request<{ mount: FolderMount }>(`/api/nodes/${nodeId}/mounts`, { method: "POST", body: JSON.stringify(body) }),
  updateMount: (nodeId: string, mountId: string, body: { name?: string; enabled?: boolean; allow_sync?: boolean }) =>
    request<{ ok: boolean }>(`/api/nodes/${nodeId}/mounts/${mountId}`, {
      method: "PATCH",
      body: JSON.stringify(body),
    }),
  deleteMount: (nodeId: string, mountId: string) =>
    request<{ ok: boolean }>(`/api/nodes/${nodeId}/mounts/${mountId}`, { method: "DELETE" }),
  pickFolder: (nodeId: string) =>
    request<{ path?: string; name?: string; kind?: "folder"; cancelled?: boolean }>(`/api/nodes/${nodeId}/pick-folder`, {
      method: "POST",
    }),
  pickFile: (nodeId: string) =>
    request<{ path?: string; name?: string; kind?: "file"; cancelled?: boolean }>(`/api/nodes/${nodeId}/pick-file`, {
      method: "POST",
    }),

  listFiles: (nodeId: string, mountId: string, relPath = "") => {
    const q = new URLSearchParams({ path: relPath });
    return request<ListResponse>(`/api/vfs/nodes/${nodeId}/mounts/${mountId}/list?${q.toString()}`);
  },
  fileUrl: (nodeId: string, mountId: string, relPath: string, download = false) => {
    const q = new URLSearchParams({ path: relPath });
    if (download) q.set("download", "1");
    return `/api/vfs/nodes/${nodeId}/mounts/${mountId}/file?${q.toString()}`;
  },
  thumbUrl: (nodeId: string, mountId: string, relPath: string) => {
    const q = new URLSearchParams({ path: relPath });
    return `/api/vfs/nodes/${nodeId}/mounts/${mountId}/thumb?${q.toString()}`;
  },
  uploadFile: async (nodeId: string, mountId: string, relPath: string, file: File) => {
    const q = new URLSearchParams({ path: relPath });
    const res = await fetch(`/api/vfs/nodes/${nodeId}/mounts/${mountId}/file?${q.toString()}`, {
      method: "PUT",
      credentials: "same-origin",
      body: file,
    });
    if (!res.ok) {
      const text = await res.text();
      let msg = `Upload failed (${res.status})`;
      try {
        msg = JSON.parse(text).error || msg;
      } catch {}
      throw new Error(msg);
    }
    return res.json();
  },
  mkdir: (nodeId: string, mountId: string, relPath: string) => {
    const q = new URLSearchParams({ path: relPath });
    return request<{ path: string }>(`/api/vfs/nodes/${nodeId}/mounts/${mountId}/mkdir?${q.toString()}`, {
      method: "POST",
    });
  },
  deleteEntry: (nodeId: string, mountId: string, relPath: string) => {
    const q = new URLSearchParams({ path: relPath });
    return request<{ ok: boolean }>(`/api/vfs/nodes/${nodeId}/mounts/${mountId}/entry?${q.toString()}`, {
      method: "DELETE",
    });
  },
  revealLocal: (nodeId: string, mountId: string, relPath: string) => {
    const q = new URLSearchParams({ path: relPath });
    return request<{ ok: boolean }>(`/api/vfs/nodes/${nodeId}/mounts/${mountId}/reveal?${q.toString()}`, {
      method: "POST",
    });
  },

  listSyncSubscriptions: () => request<{ subscriptions: SyncSubscription[] }>("/api/sync/subscriptions"),
  createSyncSubscription: (body: {
    source_node_id: string;
    source_mount_id: string;
    target_node_id: string;
    target_path: string;
  }) => request<{ id: string }>("/api/sync/subscriptions", { method: "POST", body: JSON.stringify(body) }),
  deleteSyncSubscription: (id: string) =>
    request<{ ok: boolean }>(`/api/sync/subscriptions/${encodeURIComponent(id)}`, { method: "DELETE" }),

  listTags: () => request<{ tags: TagSummary[] }>("/api/tags"),
  createTag: (body: { slug: string; description?: string }) =>
    request<{ tag: TagSummary }>("/api/tags", { method: "POST", body: JSON.stringify(body) }),
  attachTag: (body: { tag_slug: string; node_id: string; mount_id: string; rel_path: string; is_dir: boolean }) =>
    request<{ tag: TagSummary }>("/api/tags/attach", { method: "POST", body: JSON.stringify(body) }),
  detachTag: (body: { tag_slug: string; mount_id: string; rel_path: string }) =>
    request<{ ok: boolean }>("/api/tags/detach", { method: "DELETE", body: JSON.stringify(body) }),
  getTree: (slug: string) => request<{ slug: string; items: TreeItem[] }>(`/api/trees/${encodeURIComponent(slug)}`),

  listShares: () => request<{ shares: ShareLink[] }>("/api/shares"),
  checkShareSlug: (slug: string) =>
    request<{ slug: string; available: boolean; reason: string }>(
      `/api/shares/check-slug?slug=${encodeURIComponent(slug)}`,
    ),
  createShare: (body: {
    node_id: string;
    mount_id: string;
    rel_path: string;
    is_dir: boolean;
    title: string;
    mime_type: string;
    size_bytes: number;
    custom_slug?: string;
    password?: string;
    max_views?: number;
    expires_in_hours?: number;
  }) => request<{ share: ShareLink }>("/api/shares", { method: "POST", body: JSON.stringify(body) }),
  deleteShare: (slug: string) => request<{ ok: boolean }>(`/api/shares/${encodeURIComponent(slug)}`, { method: "DELETE" }),

  listAnnouncements: (all = false) =>
    request<{ announcements: Announcement[] }>(`/api/announcements${all ? "?all=1" : ""}`),
  createAnnouncement: (body: {
    title?: string;
    body: string;
    kind: AnnouncementKind;
    dismissible: boolean;
    expires_in_hours?: number;
  }) => request<{ announcement: Announcement }>("/api/announcements", { method: "POST", body: JSON.stringify(body) }),
  deleteAnnouncement: (id: string) =>
    request<{ ok: boolean }>(`/api/announcements/${encodeURIComponent(id)}`, { method: "DELETE" }),

  getPublicSettings: () => request<PublicNetworkConfig>("/api/settings/public"),
  getAdminSettings: () => request<HubSettings>("/api/settings"),
  saveAdminSettings: (body: HubSettings) =>
    request<HubSettings>("/api/settings", { method: "PATCH", body: JSON.stringify(body) }),
  testEdgeStorage: () => request<{ ok: boolean }>("/api/settings/test-edge", { method: "POST" }),
  sendP2POffer: (nodeId: string, sdp: string) =>
    request<{ sdp: string }>(`/api/p2p/nodes/${encodeURIComponent(nodeId)}/offer`, {
      method: "POST",
      body: JSON.stringify({ sdp }),
    }),

  listComputeNodes: () => request<{ nodes: NodeView[] }>("/api/compute/nodes"),
  listComputeJobs: () => request<{ jobs: ComputeJob[] }>("/api/compute/jobs"),
  convertOnAgent: async (nodeId: string, file: File, format: string, preset: string) => {
    const q = new URLSearchParams({
      node_id: nodeId,
      format,
      preset,
      filename: file.name,
    });
    const res = await fetch(`/api/compute/convert?${q.toString()}`, {
      method: "POST",
      credentials: "same-origin",
      body: file,
    });
    if (!res.ok) {
      const txt = await res.text();
      let msg = `Conversion failed (${res.status})`;
      try {
        msg = JSON.parse(txt).error || msg;
      } catch {}
      throw new Error(msg);
    }
    const blob = await res.blob();
    const baseName = file.name.replace(/\.[^.]+$/, "");
    return {
      blob,
      filename: `${baseName}.${format}`,
      encoder: res.headers.get("X-Ourchive-Encoder") || "ffmpeg",
      durationMs: Number(res.headers.get("X-Ourchive-Duration-Ms") || "0"),
    };
  },
};

export function formatBytes(bytes: number): string {
  if (bytes === 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const i = Math.min(units.length - 1, Math.floor(Math.log(bytes) / Math.log(1024)));
  const val = bytes / 1024 ** i;
  return `${val < 10 && i > 0 ? val.toFixed(1) : Math.round(val)} ${units[i]}`;
}

export function formatDate(iso: string): string {
  try {
    return new Date(iso).toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" });
  } catch {
    return iso;
  }
}

export function formatMbps(bps: number): string {
  const mbps = (bps * 8) / 1_000_000;
  if (mbps < 0.05) return "0 Mbps";
  if (mbps < 10) return `${mbps.toFixed(1)} Mbps`;
  return `${Math.round(mbps)} Mbps`;
}

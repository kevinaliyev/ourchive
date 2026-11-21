/**
 * ExplorerView — Archive browser supporting the bird's-eye Archive Root, segregated Nodes,
 * and tag-composed Shared Trees. Offers List, Grid, and Gallery (inline preview) layouts,
 * WebRTC P2P downloads, local reveal on own host, drag-and-drop uploads, and dual-consent mirroring.
 */
import { DragEvent, FormEvent, useCallback, useEffect, useId, useRef, useState } from "react";
import {
  ArrowLeft,
  Check,
  ChevronDown,
  ChevronRight,
  Columns2,
  Download,
  Eye,
  File as FileIcon,
  FileAudio,
  FileImage,
  FileText,
  FileVideo,
  Folder,
  FolderOpen,
  FolderPlus,
  Globe,
  Hash,
  LayoutGrid,
  Link2,
  List,
  Maximize2,
  Pencil,
  Plus,
  RefreshCw,
  Search,
  Trash2,
  Upload,
  X,
} from "lucide-react";
import {
  api,
  FileEntry,
  formatBytes,
  formatDate,
  formatMbps,
  NodeView,
  PublicNetworkConfig,
  ShareLink,
  SyncSubscription,
  TagSummary,
  TreeItem,
  User,
} from "../api";
import { downloadFileSmart, downloadZipSmart, uploadFileSmart } from "../p2p";
import { clearResumableDownload, listResumableDownloads, ResumableDownloadMeta } from "../transferCache";
import { CreateShareModal, ShareTarget } from "./CreateShareModal";
import { FilePreviewModal, PreviewTarget } from "./FilePreviewModal";
import { ModalDialog } from "./ModalDialog";

interface ExplorerViewProps {
  currentUser: User;
  nodes: NodeView[];
  shares: ShareLink[];
  netConfig: PublicNetworkConfig | null;
  onSharesChanged: () => Promise<void>;
  onGoToContribution: () => void;
}

type BrowseMode = "nodes" | "trees";
type ViewStyle = "table" | "grid" | "gallery";
type TypeFilter = "all" | "folders" | "video" | "audio" | "images" | "docs";

interface SavedHub {
  name: string;
  url: string;
}

const SAVED_HUBS_KEY = "ourchive-saved-hubs";

function loadSavedHubs(): SavedHub[] {
  try {
    const raw = localStorage.getItem(SAVED_HUBS_KEY);
    const parsed = raw ? (JSON.parse(raw) as SavedHub[]) : [];
    const currentOrigin = window.location.origin;
    const hasCurrent = parsed.some((h) => h.url.replace(/\/+$/, "") === currentOrigin);
    if (!hasCurrent) {
      return [{ name: window.location.host, url: currentOrigin }, ...parsed];
    }
    return parsed;
  } catch {
    return [{ name: window.location.host, url: window.location.origin }];
  }
}
type SortBy = "name" | "newest" | "largest";

export function ExplorerView({
  currentUser,
  nodes,
  shares,
  netConfig,
  onSharesChanged,
  onGoToContribution,
}: ExplorerViewProps) {
  const [mode, setMode] = useState<BrowseMode>("nodes");
  const [savedHubs, setSavedHubs] = useState<SavedHub[]>(loadSavedHubs);
  const [addHubOpen, setAddHubOpen] = useState(false);
  const [newHubName, setNewHubName] = useState("");
  const [newHubUrl, setNewHubUrl] = useState("");

  function handleSaveHub(e: FormEvent) {
    e.preventDefault();
    const trimmed = newHubUrl.trim().replace(/\/+$/, "");
    if (!trimmed) return;
    let normalized = trimmed;
    if (!/^https?:\/\//i.test(normalized)) {
      normalized = "https://" + normalized;
    }
    let hostLabel = newHubName.trim();
    if (!hostLabel) {
      try {
        hostLabel = new URL(normalized).host;
      } catch {
        hostLabel = normalized;
      }
    }
    const next = [...savedHubs.filter((h) => h.url.replace(/\/+$/, "") !== normalized), { name: hostLabel, url: normalized }];
    setSavedHubs(next);
    try {
      localStorage.setItem(SAVED_HUBS_KEY, JSON.stringify(next));
    } catch {}
    setNewHubName("");
    setNewHubUrl("");
    setAddHubOpen(false);
  }

  function handleRemoveHub(url: string) {
    const next = savedHubs.filter((h) => h.url !== url);
    setSavedHubs(next);
    try {
      localStorage.setItem(SAVED_HUBS_KEY, JSON.stringify(next));
    } catch {}
  }
  const [viewStyle, setViewStyle] = useState<ViewStyle>("table");
  const [searchQuery, setSearchQuery] = useState("");
  const [typeFilter, setTypeFilter] = useState<TypeFilter>("all");
  const [sortBy, setSortBy] = useState<SortBy>("name");
  const [expandedDirs, setExpandedDirs] = useState<Record<string, FileEntry[]>>({});
  const [expandingPaths, setExpandingPaths] = useState<Record<string, boolean>>({});
  const [gallerySelectedPath, setGallerySelectedPath] = useState<string>("");

  // Empty nodeId / mountId = Archive Root (bird's-eye view across all contributors)
  const [selectedNodeId, setSelectedNodeId] = useState<string>("");
  const [selectedMountId, setSelectedMountId] = useState<string>("");
  const [relPath, setRelPath] = useState<string>("");

  // Opt-in Parity Sync subscriptions
  const [syncSubs, setSyncSubs] = useState<SyncSubscription[]>([]);
  const [syncModalOpen, setSyncModalOpen] = useState(false);
  const [syncTargetNodeId, setSyncTargetNodeId] = useState("");
  const [syncTargetPath, setSyncTargetPath] = useState("");

  const [entries, setEntries] = useState<FileEntry[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [dragging, setDragging] = useState(false);
  const [uploadStatus, setUploadStatus] = useState<string | null>(null);

  // Tags / Shared Trees
  const [tags, setTags] = useState<TagSummary[]>([]);
  const [selectedTreeSlug, setSelectedTreeSlug] = useState<string>("");
  const [treeItems, setTreeItems] = useState<TreeItem[]>([]);
  const [returnTreeSlug, setReturnTreeSlug] = useState<string | null>(null);
  const [newTreeOpen, setNewTreeOpen] = useState(false);
  const [newTreeSlug, setNewTreeSlug] = useState("");
  const [newTreeDesc, setNewTreeDesc] = useState("");

  // New folder modal
  const [mkdirOpen, setMkdirOpen] = useState(false);
  const [newFolderName, setNewFolderName] = useState("");

  // Multi-select & Global Search across all online nodes
  const [resumableList, setResumableList] = useState<ResumableDownloadMeta[]>(() => listResumableDownloads());
  const [selectedPaths, setSelectedPaths] = useState<string[]>([]);

  useEffect(() => {
    const sync = () => setResumableList(listResumableDownloads());
    window.addEventListener("ourchive-transfers-changed", sync);
    return () => window.removeEventListener("ourchive-transfers-changed", sync);
  }, []);
  const [globalSearchOpen, setGlobalSearchOpen] = useState(false);
  const [globalQuery, setGlobalQuery] = useState("");
  const [globalSearching, setGlobalSearching] = useState(false);
  const [globalHits, setGlobalHits] = useState<
    {
      node_id: string;
      node_name: string;
      is_own_host: boolean;
      mount_id: string;
      mount_name: string;
      name: string;
      path: string;
      is_dir: boolean;
      size: number;
      mod_time: string;
      mime_type: string;
    }[]
  >([]);

  function toggleSelectPath(p: string) {
    setSelectedPaths((prev) => (prev.includes(p) ? prev.filter((x) => x !== p) : [...prev, p]));
  }

  async function handleGlobalSearch(e: FormEvent) {
    e.preventDefault();
    if (!globalQuery.trim()) return;
    setGlobalSearching(true);
    try {
      const res = await api.searchFiles(globalQuery.trim());
      setGlobalHits(res.hits);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Global search failed");
    } finally {
      setGlobalSearching(false);
    }
  }

  async function handleBulkDelete() {
    if (!selectedNode || !selectedMount || selectedPaths.length === 0) return;
    if (!window.confirm(`Delete ${selectedPaths.length} selected item(s) from ${selectedNode.display_name}?`)) return;
    for (const p of selectedPaths) {
      try {
        await api.deleteEntry(selectedNode.id, selectedMount.id, p);
      } catch (err) {
        setError(err instanceof Error ? err.message : `Failed to delete ${p}`);
        break;
      }
    }
    setSelectedPaths([]);
    await loadDirectory();
  }

  // On-demand slide-over Inspector drawer, Rich File Previewer & Share Modal
  const [inspected, setInspected] = useState<FileEntry | null>(null);
  const [previewTarget, setPreviewTarget] = useState<PreviewTarget | null>(null);
  const [shareTarget, setShareTarget] = useState<ShareTarget | null>(null);
  const [copiedInspectedLink, setCopiedInspectedLink] = useState(false);
  const [tagInput, setTagInput] = useState("");
  const drawerRef = useRef<HTMLDialogElement>(null);
  const drawerTitleId = useId();
  const fileInputRef = useRef<HTMLInputElement>(null);

  const selectedNode = selectedNodeId ? nodes.find((n) => n.id === selectedNodeId) ?? null : null;
  const availableMounts = selectedNode?.mounts.filter((m) => m.enabled && !m.rejected) ?? [];
  const selectedMount = selectedMountId ? availableMounts.find((m) => m.id === selectedMountId) ?? null : null;
  const atArchiveRoot = !selectedMount;
  const isOwnHost = Boolean(selectedNode && selectedNode.owner_id === currentUser.id);

  const activeNodeId = selectedNode?.id ?? "";
  const activeNodeStatus = selectedNode?.status ?? "offline";
  const activeMountId = selectedMount?.id ?? "";

  const loadSyncSubs = useCallback(async () => {
    try {
      const res = await api.listSyncSubscriptions();
      setSyncSubs(res.subscriptions);
    } catch {}
  }, []);

  useEffect(() => {
    void loadSyncSubs();
  }, [loadSyncSubs]);

  const loadTags = useCallback(async () => {
    try {
      const res = await api.listTags();
      setTags(res.tags);
      if (!selectedTreeSlug && res.tags.length > 0) {
        setSelectedTreeSlug(res.tags[0].slug);
      }
    } catch {}
  }, [selectedTreeSlug]);

  useEffect(() => {
    void loadTags();
  }, [loadTags]);

  const loadDirectory = useCallback(async () => {
    if (!activeNodeId || !activeMountId) {
      setEntries([]);
      return;
    }
    if (activeNodeStatus !== "online") {
      setEntries([]);
      setError(activeNodeStatus === "paused" ? "Sharing is paused on this machine." : "This machine is currently offline.");
      return;
    }
    setLoading(true);
    setError(null);
    try {
      const res = await api.listFiles(activeNodeId, activeMountId, relPath);
      setEntries(res.entries);
    } catch (err) {
      setEntries([]);
      setError(err instanceof Error ? err.message : "Failed to load folder");
    } finally {
      setLoading(false);
    }
  }, [activeNodeId, activeNodeStatus, activeMountId, relPath]);

  useEffect(() => {
    if (mode === "nodes") {
      setExpandedDirs({});
      void loadDirectory();
    }
  }, [mode, loadDirectory]);

  async function toggleExpandDir(dirPath: string) {
    if (!activeNodeId || !activeMountId) return;
    if (expandedDirs[dirPath]) {
      setExpandedDirs((prev) => {
        const next = { ...prev };
        delete next[dirPath];
        // Also collapse any nested children under this folder.
        for (const k of Object.keys(next)) {
          if (k.startsWith(dirPath + "/")) delete next[k];
        }
        return next;
      });
      return;
    }
    setExpandingPaths((prev) => ({ ...prev, [dirPath]: true }));
    try {
      const res = await api.listFiles(activeNodeId, activeMountId, dirPath);
      setExpandedDirs((prev) => ({ ...prev, [dirPath]: res.entries }));
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to expand folder");
    } finally {
      setExpandingPaths((prev) => {
        const next = { ...prev };
        delete next[dirPath];
        return next;
      });
    }
  }

  function matchesFilter(entry: FileEntry): boolean {
    const q = searchQuery.trim().toLowerCase();
    if (q && !entry.name.toLowerCase().includes(q) && !(entry.tags ?? []).some((t) => t.includes(q))) {
      return false;
    }
    if (typeFilter === "all") return true;
    if (typeFilter === "folders") return entry.is_dir;
    if (entry.is_dir) return false;
    if (typeFilter === "video") return entry.mime_type.startsWith("video/");
    if (typeFilter === "audio") return entry.mime_type.startsWith("audio/");
    if (typeFilter === "images") return entry.mime_type.startsWith("image/");
    if (typeFilter === "docs") {
      return (
        entry.mime_type === "application/pdf" ||
        entry.mime_type.startsWith("text/") ||
        entry.mime_type === "application/json"
      );
    }
    return true;
  }

  function sortEntries(list: FileEntry[]): FileEntry[] {
    return [...list].sort((a, b) => {
      if (a.is_dir !== b.is_dir) return a.is_dir ? -1 : 1;
      if (sortBy === "newest") return b.mod_time.localeCompare(a.mod_time);
      if (sortBy === "largest") return b.size - a.size;
      return a.name.toLowerCase().localeCompare(b.name.toLowerCase());
    });
  }

  // Flatten visible rows for Table view, including inline-expanded folders.
  const visibleTableRows: Array<{ entry: FileEntry; depth: number }> = [];
  function collectRows(list: FileEntry[], depth: number) {
    for (const entry of sortEntries(list)) {
      const children = entry.is_dir ? expandedDirs[entry.path] : undefined;
      if (matchesFilter(entry) || (children && children.length > 0)) {
        visibleTableRows.push({ entry, depth });
      }
      if (children) {
        collectRows(children, depth + 1);
      }
    }
  }
  collectRows(entries, 0);

  const visibleGridEntries = sortEntries(entries.filter(matchesFilter));
  const galleryEntry =
    visibleGridEntries.find((e) => e.path === gallerySelectedPath) ??
    visibleGridEntries.find((e) => !e.is_dir) ??
    visibleGridEntries[0] ??
    null;

  const myNodes = nodes.filter((n) => n.owner_id === currentUser.id);
  const activeMountSub = selectedMount
    ? syncSubs.find((s) => s.source_mount_id === selectedMount.id && s.enabled)
    : undefined;

  async function handleRevealLocal(nodeId: string, mountId: string, path: string) {
    try {
      await api.revealLocal(nodeId, mountId, path);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not reveal file on local machine");
    }
  }

  async function handleSubscribeSync(e: FormEvent) {
    e.preventDefault();
    if (!selectedNode || !selectedMount || !syncTargetNodeId || !syncTargetPath.trim()) return;
    try {
      await api.createSyncSubscription({
        source_node_id: selectedNode.id,
        source_mount_id: selectedMount.id,
        target_node_id: syncTargetNodeId,
        target_path: syncTargetPath.trim(),
      });
      setSyncModalOpen(false);
      await loadSyncSubs();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not enable parity sync");
    }
  }

  async function handleUnsubscribeSync(subId: string) {
    await api.deleteSyncSubscription(subId);
    await loadSyncSubs();
  }

  const loadTree = useCallback(async () => {
    if (!selectedTreeSlug) {
      setTreeItems([]);
      return;
    }
    setLoading(true);
    setError(null);
    try {
      const res = await api.getTree(selectedTreeSlug);
      setTreeItems(res.items);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to load shared tree");
    } finally {
      setLoading(false);
    }
  }, [selectedTreeSlug]);

  useEffect(() => {
    if (mode === "trees") {
      void loadTree();
    }
  }, [mode, loadTree]);

  // Open/close native <dialog> Inspector drawer on demand.
  useEffect(() => {
    const dlg = drawerRef.current;
    if (!dlg) return;
    if (inspected && !dlg.open) {
      dlg.showModal();
    } else if (!inspected && dlg.open) {
      dlg.close();
    }
  }, [inspected]);

  async function handleUploadFiles(fileList: FileList | File[]) {
    if (!selectedNode || !selectedMount) return;
    const files = Array.from(fileList);
    if (files.length === 0) return;
    setError(null);
    let lastTransport: "p2p" | "relay" = "p2p";
    for (let i = 0; i < files.length; i++) {
      const f = files[i];
      const targetPath = relPath ? `${relPath}/${f.name}` : f.name;
      setUploadStatus(`Connecting P2P to upload ${f.name} (${i + 1}/${files.length})…`);
      try {
        const res = await uploadFileSmart(selectedNode.id, selectedMount.id, targetPath, f, netConfig, (p) => {
          if (p.transport === "p2p" && p.total > 0) {
            const pct = Math.min(100, Math.round((p.received / p.total) * 100));
            setUploadStatus(`Direct P2P upload · ${f.name} (${pct}%)`);
          } else if (p.transport === "relay") {
            setUploadStatus(`Uploading ${f.name} via Hub relay fallback…`);
          }
        });
        lastTransport = res.transport;
      } catch (err) {
        setError(err instanceof Error ? err.message : `Failed to upload ${f.name}`);
        break;
      }
    }
    setUploadStatus(
      lastTransport === "p2p" ? `Uploaded ${files.length} file${files.length === 1 ? "" : "s"} directly via P2P (0 Hub egress).` : null,
    );
    if (lastTransport === "p2p") window.setTimeout(() => setUploadStatus(null), 2500);
    await loadDirectory();
  }

  async function handleSmartZip(nodeId: string, mountId: string, targetRelPath: string, label: string, paths?: string[]) {
    setError(null);
    setUploadStatus(`Connecting P2P to stream ${label}.zip…`);
    try {
      const modeUsed = await downloadZipSmart(
        nodeId,
        mountId,
        targetRelPath,
        netConfig,
        (p) => {
          if (p.transport === "p2p") {
            setUploadStatus(`Direct P2P .zip · ${label} (${formatBytes(p.received)})`);
          } else {
            setUploadStatus(`Downloading ${label}.zip via Hub relay fallback…`);
          }
        },
        paths,
      );
      setUploadStatus(modeUsed === "p2p" ? `Downloaded ${label}.zip directly via P2P (0 Hub egress).` : null);
      if (modeUsed === "p2p") window.setTimeout(() => setUploadStatus(null), 2500);
    } catch (err) {
      setUploadStatus(null);
      setError(err instanceof Error ? err.message : "ZIP download failed");
    }
  }

  async function handleRenameEntry(entry: FileEntry) {
    if (!selectedNode || !selectedMount) return;
    const nextName = window.prompt("Rename or move (relative path inside folder):", entry.path);
    if (!nextName || !nextName.trim() || nextName.trim() === entry.path) return;
    try {
      await api.renameEntry(selectedNode.id, selectedMount.id, entry.path, nextName.trim());
      await loadDirectory();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Rename failed");
    }
  }

  function handleDrop(e: DragEvent) {
    e.preventDefault();
    setDragging(false);
    if (mode === "nodes" && selectedNode?.is_owner && selectedNode.status === "online" && e.dataTransfer.files.length > 0) {
      void handleUploadFiles(e.dataTransfer.files);
    }
  }

  async function handleMkdir(e: FormEvent) {
    e.preventDefault();
    if (!selectedNode || !selectedMount || !newFolderName.trim()) return;
    const target = relPath ? `${relPath}/${newFolderName.trim()}` : newFolderName.trim();
    try {
      await api.mkdir(selectedNode.id, selectedMount.id, target);
      setNewFolderName("");
      setMkdirOpen(false);
      await loadDirectory();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not create folder");
    }
  }

  async function handleSmartDownload(nodeId: string, mountId: string, entryPath: string, name: string) {
    setError(null);
    setUploadStatus(`Connecting P2P to download ${name}…`);
    try {
      const modeUsed = await downloadFileSmart(nodeId, mountId, entryPath, netConfig, (p) => {
        if (p.transport === "p2p" && p.total > 0) {
          const pct = Math.min(100, Math.round((p.received / p.total) * 100));
          setUploadStatus(`Direct P2P download · ${name} (${pct}%)`);
        } else if (p.transport === "relay") {
          setUploadStatus(`Downloading ${name} via Hub relay fallback…`);
        }
      });
      setUploadStatus(modeUsed === "p2p" ? `Downloaded ${name} directly via P2P (0 Hub egress).` : null);
      if (modeUsed === "p2p") window.setTimeout(() => setUploadStatus(null), 2500);
    } catch (err) {
      setUploadStatus(null);
      setError(err instanceof Error ? err.message : "Download failed");
    }
  }

  async function handleDeleteEntry(entry: FileEntry) {
    if (!selectedNode || !selectedMount) return;
    if (!window.confirm(`Delete "${entry.name}" from ${selectedNode.display_name}?`)) return;
    try {
      await api.deleteEntry(selectedNode.id, selectedMount.id, entry.path);
      setInspected(null);
      await loadDirectory();
      await loadTags();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Delete failed");
    }
  }

  async function handleAttachTag(e: FormEvent) {
    e.preventDefault();
    if (!selectedNode || !selectedMount || !inspected || !tagInput.trim()) return;
    try {
      const res = await api.attachTag({
        tag_slug: tagInput.trim(),
        node_id: selectedNode.id,
        mount_id: selectedMount.id,
        rel_path: inspected.path,
        is_dir: inspected.is_dir,
      });
      const slug = res.tag.slug;
      setInspected((prev) =>
        prev ? { ...prev, tags: Array.from(new Set([...(prev.tags ?? []), slug])) } : null,
      );
      setTagInput("");
      await loadDirectory();
      await loadTags();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to add tag");
    }
  }

  async function handleDetachTag(slug: string) {
    if (!selectedMount || !inspected) return;
    await api.detachTag({ tag_slug: slug, mount_id: selectedMount.id, rel_path: inspected.path });
    setInspected((prev) => (prev ? { ...prev, tags: (prev.tags ?? []).filter((t) => t !== slug) } : null));
    await loadDirectory();
    await loadTags();
  }

  async function handleCreateTree(e: FormEvent) {
    e.preventDefault();
    if (!newTreeSlug.trim()) return;
    try {
      const res = await api.createTag({ slug: newTreeSlug.trim(), description: newTreeDesc.trim() });
      setNewTreeSlug("");
      setNewTreeDesc("");
      setNewTreeOpen(false);
      await loadTags();
      setSelectedTreeSlug(res.tag.slug);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not create shared tree");
    }
  }

  function openTreeItem(item: TreeItem) {
    if (item.is_dir) {
      setReturnTreeSlug(item.tag_slug);
      setSelectedNodeId(item.node_id);
      setSelectedMountId(item.mount_id);
      setRelPath(item.rel_path);
      setMode("nodes");
    } else if (item.node_status === "online") {
      const name = item.rel_path.split("/").pop() || item.rel_path;
      setPreviewTarget({
        nodeId: item.node_id,
        nodeName: item.node_name,
        mountId: item.mount_id,
        entry: {
          name,
          path: item.rel_path,
          is_dir: false,
          size: 0,
          mod_time: item.created_at,
          mime_type: guessMimeFromName(name),
        },
      });
    }
  }

  const fileEntries = entries.filter((e) => !e.is_dir);
  const previewIndex =
    previewTarget && selectedNode && selectedMount && previewTarget.mountId === selectedMount.id
      ? fileEntries.findIndex((e) => e.path === previewTarget.entry.path)
      : -1;

  function openPreview(entry: FileEntry) {
    if (!selectedNode || !selectedMount || entry.is_dir) return;
    setPreviewTarget({
      nodeId: selectedNode.id,
      nodeName: selectedNode.display_name,
      mountId: selectedMount.id,
      entry,
    });
  }

  function findExistingShare(mountId: string, path: string): ShareLink | undefined {
    return shares.find((s) => !s.expired && s.mount_id === mountId && s.rel_path === path);
  }

  function openShareForEntry(node: NodeView, mountId: string, entry: FileEntry) {
    setShareTarget({
      nodeId: node.id,
      nodeName: node.display_name,
      mountId,
      relPath: entry.path,
      isDir: entry.is_dir,
      title: entry.name,
      mimeType: entry.mime_type,
      sizeBytes: entry.size,
    });
  }

  const inspectedShare = inspected && selectedMount ? findExistingShare(selectedMount.id, inspected.path) : undefined;
  const previewShare = previewTarget ? findExistingShare(previewTarget.mountId, previewTarget.entry.path) : undefined;

  const breadcrumbs = relPath ? relPath.split("/").filter(Boolean) : [];

  if (nodes.length === 0) {
    return (
      <section className="card empty-card">
        <Folder size={24} strokeWidth={1.4} className="muted-icon" />
        <div className="empty-title">No contributor machines connected</div>
        <p className="empty-copy">
          Connect a machine in Contribution settings and add a shared folder to start browsing your archive.
        </p>
        <button type="button" className="btn btn-primary" onClick={onGoToContribution}>
          Open contribution settings
        </button>
      </section>
    );
  }

  return (
    <div className="section-stack">
      {/* Segregated Hubs Bar */}
      <div className="hub-strip" role="navigation" aria-label="Segregated Ourchive Hubs">
        <span className="hub-strip-label">
          <Globe size={13} strokeWidth={1.6} />
          <span>Hubs</span>
        </span>
        <div className="pill-strip">
          {savedHubs.map((h) => {
            const isCurrent = h.url.replace(/\/+$/, "") === window.location.origin;
            return (
              <span key={h.url} className="hub-pill-wrap">
                <a
                  href={h.url}
                  className="source-pill"
                  aria-current={isCurrent ? "page" : undefined}
                  aria-pressed={isCurrent}
                  onClick={(e) => {
                    if (isCurrent) e.preventDefault();
                  }}
                >
                  <span className={`dot ${isCurrent ? "" : "paused"}`} />
                  <span>{h.name}</span>
                </a>
                {!isCurrent && (
                  <button
                    type="button"
                    className="hub-remove-btn"
                    title={`Remove ${h.name}`}
                    aria-label={`Remove hub ${h.name}`}
                    onClick={() => handleRemoveHub(h.url)}
                  >
                    <X size={11} />
                  </button>
                )}
              </span>
            );
          })}
          <button type="button" className="btn btn-sm" onClick={() => setAddHubOpen(true)}>
            <Plus size={13} strokeWidth={1.6} />
            <span>Add hub</span>
          </button>
        </div>
      </div>

      {/* Top Mode & Source Strip */}
      <div className="explorer-top">
        <div className="segmented" role="group" aria-label="Browse mode">
          <button
            type="button"
            className="seg-btn"
            aria-pressed={mode === "nodes"}
            onClick={() => {
              setMode("nodes");
              setReturnTreeSlug(null);
            }}
          >
            Nodes
          </button>
          <button
            type="button"
            className="seg-btn"
            aria-pressed={mode === "trees"}
            onClick={() => setMode("trees")}
          >
            Shared trees
          </button>
        </div>

        {mode === "nodes" ? (
          <div className="pill-strip" role="group" aria-label="Archive scope">
            <button
              type="button"
              className="source-pill"
              aria-pressed={selectedNodeId === "" && selectedMountId === ""}
              onClick={() => {
                setSelectedNodeId("");
                setSelectedMountId("");
                setRelPath("");
                setReturnTreeSlug(null);
              }}
            >
              <span>All (Archive)</span>
            </button>
            {nodes.map((n) => {
              const isSelected = selectedNodeId === n.id;
              const isOffline = n.status === "offline";
              return (
                <button
                  key={n.id}
                  type="button"
                  className={`source-pill ${isOffline ? "offline" : ""}`}
                  aria-pressed={isSelected}
                  onClick={() => {
                    setSelectedNodeId(n.id);
                    setSelectedMountId("");
                    setRelPath("");
                    setReturnTreeSlug(null);
                  }}
                >
                  <span className={`dot ${isOffline ? "offline" : n.status === "paused" ? "paused" : ""}`} />
                  <span>{n.display_name}</span>
                </button>
              );
            })}
          </div>
        ) : (
          <div className="pill-strip" role="group" aria-label="Shared trees">
            {tags.map((t) => (
              <button
                key={t.id}
                type="button"
                className="source-pill"
                aria-pressed={selectedTreeSlug === t.slug}
                onClick={() => setSelectedTreeSlug(t.slug)}
              >
                <Hash size={13} strokeWidth={1.6} />
                <span>{t.slug}</span>
                <span className="pill-count mono">{t.item_count}</span>
              </button>
            ))}
            <button type="button" className="btn" onClick={() => setNewTreeOpen(true)}>
              <Plus size={14} strokeWidth={1.6} />
              <span>New tree</span>
            </button>
          </div>
        )}
      </div>

      {/* Archive Root Overview (Higher Perspective Across All Contributors) */}
      {mode === "nodes" && atArchiveRoot && (
        <section className="card section-stack">
          <div className="section-head" style={{ marginBottom: 0 }}>
            <div>
              <h2 className="section-title">
                {selectedNode ? `${selectedNode.display_name} · Shared roots` : "Archive root"}
              </h2>
              <p className="page-sub">
                {selectedNode
                  ? `Shared folders and individual files hosted on ${selectedNode.display_name}.`
                  : "All shared folders and file references across every contributor machine."}
              </p>
            </div>
            <button type="button" className="btn" onClick={onGoToContribution}>
              Manage contributions
            </button>
          </div>

          {(() => {
            const scopedNodes = selectedNode ? [selectedNode] : nodes;
            const rootItems = scopedNodes.flatMap((n) =>
              n.mounts
                .filter((m) => m.enabled && !m.rejected)
                .map((m) => ({ node: n, mount: m })),
            );
            if (rootItems.length === 0) {
              return (
                <div className="mounts-empty">
                  No shared folders or files yet. Add a folder or individual file in Contribution settings.
                </div>
              );
            }
            return (
              <div className="archive-root-grid">
                {rootItems.map(({ node, mount }) => {
                  const isOff = node.status === "offline";
                  const sub = syncSubs.find((s) => s.source_mount_id === mount.id && s.enabled);
                  return (
                    <button
                      key={`${node.id}-${mount.id}`}
                      type="button"
                      className={`archive-root-card ${isOff ? "offline" : ""}`}
                      onClick={() => {
                        setSelectedNodeId(node.id);
                        setSelectedMountId(mount.id);
                        setRelPath("");
                      }}
                    >
                      <div className="archive-root-top">
                        <div className="node-title-row">
                          {mount.kind === "file" ? (
                            <FileIcon size={17} strokeWidth={1.5} className="icon-folder" />
                          ) : (
                            <Folder size={17} strokeWidth={1.5} className="icon-folder" />
                          )}
                          <strong>{mount.name}</strong>
                        </div>
                        <div className="node-title-row">
                          {mount.allow_sync && (
                            <span className="tag-chip" title="Contributor allows opt-in local mirroring">
                              {sub ? "Mirroring" : "Mirrorable"}
                            </span>
                          )}
                          <span className="tag-chip mono">{mount.kind === "file" ? "File" : "Folder"}</span>
                        </div>
                      </div>
                      <div className="archive-root-meta">
                        <span className="node-title-row">
                          <span className={`dot ${isOff ? "offline" : node.status === "paused" ? "paused" : ""}`} />
                          <span>
                            {node.display_name}
                            {node.owner_id === currentUser.id ? " (This PC)" : ""}
                          </span>
                        </span>
                        <span className="io-pair">
                          <span className="io-up">↑ {formatMbps(node.traffic.upload_bps)}</span>
                          <span className="io-down">↓ {formatMbps(node.traffic.download_bps)}</span>
                        </span>
                      </div>
                    </button>
                  );
                })}
              </div>
            );
          })()}
        </section>
      )}

      {/* Inside a Folder / File Mount */}
      {mode === "nodes" && !atArchiveRoot && selectedNode && selectedMount && (
        <section
          className={`card explorer-card ${dragging ? "dragging" : ""} ${selectedNode.status === "offline" ? "offline-surface" : ""}`}
          onDragOver={(e) => {
            e.preventDefault();
            if (selectedNode.is_owner && selectedNode.status === "online") setDragging(true);
          }}
          onDragLeave={() => setDragging(false)}
          onDrop={handleDrop}
        >
          {/* Mounts + Active Node Traffic */}
          <div className="explorer-bar">
            <div className="mount-tabs">
              <button
                type="button"
                className="btn btn-sm"
                onClick={() => {
                  setSelectedMountId("");
                  setRelPath("");
                }}
              >
                <ArrowLeft size={13} strokeWidth={1.6} />
                <span>Archive</span>
              </button>
              {returnTreeSlug && (
                <button
                  type="button"
                  className="btn btn-sm"
                  onClick={() => {
                    setMode("trees");
                    setSelectedTreeSlug(returnTreeSlug);
                    setReturnTreeSlug(null);
                  }}
                >
                  <ArrowLeft size={13} strokeWidth={1.6} />
                  <span>#{returnTreeSlug}</span>
                </button>
              )}
              {availableMounts.map((m) => (
                <button
                  key={m.id}
                  type="button"
                  className="mount-tab"
                  aria-current={selectedMount.id === m.id ? "true" : undefined}
                  onClick={() => {
                    setSelectedMountId(m.id);
                    setRelPath("");
                  }}
                >
                  {m.kind === "file" ? <FileIcon size={14} strokeWidth={1.5} /> : <Folder size={14} strokeWidth={1.5} />}
                  <span>{m.name}</span>
                </button>
              ))}
            </div>

            <div className="explorer-actions">
              <div className="io-pair" title={`Live traffic on ${selectedNode.display_name}`}>
                <span className="io-up">↑ {formatMbps(selectedNode.traffic.upload_bps)}</span>
                <span className="io-down">↓ {formatMbps(selectedNode.traffic.download_bps)}</span>
              </div>

              {selectedMount.allow_sync && selectedMount.kind !== "file" && !isOwnHost && (
                activeMountSub ? (
                  <button
                    type="button"
                    className="btn btn-sm"
                    title="Click to stop mirroring this folder locally"
                    onClick={() => handleUnsubscribeSync(activeMountSub.id)}
                  >
                    <RefreshCw size={13} strokeWidth={1.6} />
                    <span>Mirroring</span>
                  </button>
                ) : (
                  <button
                    type="button"
                    className="btn btn-sm"
                    onClick={() => {
                      setSyncTargetNodeId(myNodes[0]?.id ?? "");
                      setSyncTargetPath(`~/Ourchive-Sync/${selectedMount.name}`);
                      setSyncModalOpen(true);
                    }}
                  >
                    <RefreshCw size={13} strokeWidth={1.6} />
                    <span>Mirror locally</span>
                  </button>
                )
              )}

              <div className="segmented" role="group" aria-label="View layout">
                <button
                  type="button"
                  className="seg-btn"
                  aria-pressed={viewStyle === "table"}
                  aria-label="List view"
                  title="List view"
                  onClick={() => setViewStyle("table")}
                >
                  <List size={14} strokeWidth={1.6} />
                </button>
                <button
                  type="button"
                  className="seg-btn"
                  aria-pressed={viewStyle === "grid"}
                  aria-label="Grid view"
                  title="Thumbnail grid view"
                  onClick={() => setViewStyle("grid")}
                >
                  <LayoutGrid size={14} strokeWidth={1.6} />
                </button>
                <button
                  type="button"
                  className="seg-btn"
                  aria-pressed={viewStyle === "gallery"}
                  aria-label="Gallery preview view"
                  title="Gallery view with integrated preview"
                  onClick={() => setViewStyle("gallery")}
                >
                  <Columns2 size={14} strokeWidth={1.6} />
                </button>
              </div>

              <button
                type="button"
                className="btn"
                title="Search across all online machines"
                onClick={() => setGlobalSearchOpen(true)}
              >
                <Search size={14} strokeWidth={1.6} />
                <span>Global search</span>
              </button>

              {selectedNode.status === "online" && selectedMount.kind !== "file" && (
                <button
                  type="button"
                  className="btn"
                  title="Stream current folder as .zip over direct P2P (0 Hub egress)"
                  onClick={() =>
                    void handleSmartZip(
                      selectedNode.id,
                      selectedMount.id,
                      relPath,
                      relPath ? relPath.split("/").pop() || selectedMount.name : selectedMount.name,
                    )
                  }
                >
                  <Download size={14} strokeWidth={1.6} />
                  <span>Folder .zip</span>
                </button>
              )}

              {selectedNode.is_owner && selectedNode.status === "online" && selectedMount.kind !== "file" && (
                <>
                  <button type="button" className="btn" onClick={() => setMkdirOpen(true)}>
                    <FolderPlus size={14} strokeWidth={1.6} />
                    <span>New folder</span>
                  </button>
                  <button
                    type="button"
                    className="btn btn-primary"
                    onClick={() => fileInputRef.current?.click()}
                  >
                    <Upload size={14} strokeWidth={1.6} />
                    <span>Upload files</span>
                  </button>
                  <input
                    ref={fileInputRef}
                    type="file"
                    multiple
                    hidden
                    onChange={(e) => {
                      if (e.target.files) void handleUploadFiles(e.target.files);
                      e.target.value = "";
                    }}
                  />
                </>
              )}
            </div>
          </div>

          {/* Breadcrumbs + Filter / Sort Bar */}
          <div className="explorer-subbar">
            <nav className="breadcrumbs" aria-label="Folder path">
              <button
                type="button"
                className="crumb"
                onClick={() => {
                  setSelectedNodeId("");
                  setSelectedMountId("");
                  setRelPath("");
                }}
              >
                Archive
              </button>
              <span className="crumb-sep">/</span>
              <button
                type="button"
                className="crumb"
                onClick={() => {
                  setSelectedMountId("");
                  setRelPath("");
                }}
              >
                {selectedNode.display_name}
              </button>
              <span className="crumb-sep">/</span>
              <button type="button" className="crumb" onClick={() => setRelPath("")}>
                {selectedMount.name}
              </button>
              {breadcrumbs.map((part, idx) => {
                const full = breadcrumbs.slice(0, idx + 1).join("/");
                return (
                  <span key={full} className="crumb-pair">
                    <span className="crumb-sep">/</span>
                    <button type="button" className="crumb" onClick={() => setRelPath(full)}>
                      {part}
                    </button>
                  </span>
                );
              })}
            </nav>

            <div className="filter-bar">
              <div className="search-box">
                <Search size={13} strokeWidth={1.6} className="search-icon" />
                <input
                  type="search"
                  placeholder="Filter by name or #tag…"
                  aria-label="Filter files"
                  value={searchQuery}
                  onChange={(e) => setSearchQuery(e.target.value)}
                />
              </div>

              <div className="filter-chips" role="group" aria-label="Filter by type">
                {(
                  [
                    ["all", "All"],
                    ["folders", "Folders"],
                    ["video", "Video"],
                    ["audio", "Audio"],
                    ["images", "Images"],
                    ["docs", "Docs"],
                  ] as [TypeFilter, string][]
                ).map(([val, label]) => (
                  <button
                    key={val}
                    type="button"
                    className="filter-chip"
                    aria-pressed={typeFilter === val}
                    onClick={() => setTypeFilter(val)}
                  >
                    {label}
                  </button>
                ))}
              </div>

              <select
                className="sort-select"
                aria-label="Sort files"
                value={sortBy}
                onChange={(e) => setSortBy(e.target.value as SortBy)}
              >
                <option value="name">Sort: Name</option>
                <option value="newest">Sort: Newest</option>
                <option value="largest">Sort: Largest</option>
              </select>
            </div>
          </div>

          {selectedPaths.length > 0 && selectedNode && selectedMount && (
            <div className="status-banner" style={{ justifyContent: "space-between", flexWrap: "wrap", gap: 8 }}>
              <span>
                <strong>{selectedPaths.length}</strong> selected
              </span>
              <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
                <button
                  type="button"
                  className="btn btn-sm"
                  onClick={() =>
                    void handleSmartZip(selectedNode.id, selectedMount.id, "", "selection", selectedPaths)
                  }
                >
                  <Download size={13} strokeWidth={1.6} />
                  <span>Download .zip (P2P)</span>
                </button>
                {selectedNode.is_owner && (
                  <button type="button" className="btn btn-sm btn-danger" onClick={() => void handleBulkDelete()}>
                    <Trash2 size={13} strokeWidth={1.6} />
                    <span>Delete selected</span>
                  </button>
                )}
                <button type="button" className="btn btn-sm" onClick={() => setSelectedPaths([])}>
                  Clear
                </button>
              </div>
            </div>
          )}

          {resumableList.length > 0 && (
            <div className="status-banner" style={{ flexDirection: "column", alignItems: "stretch", gap: 6 }}>
              <div style={{ fontWeight: 600, fontSize: 12 }}>
                Interrupted downloads saved on disk ({resumableList.length})
              </div>
              {resumableList.map((dl) => {
                const pct = dl.totalSize > 0 ? Math.min(99, Math.round((dl.receivedBytes / dl.totalSize) * 100)) : 0;
                return (
                  <div
                    key={dl.key}
                    style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 8, flexWrap: "wrap" }}
                  >
                    <span className="mono" style={{ fontSize: 12 }}>
                      {dl.name} · {formatBytes(dl.receivedBytes)}
                      {dl.totalSize > 0 ? ` / ${formatBytes(dl.totalSize)} (${pct}%)` : ""}
                    </span>
                    <div style={{ display: "flex", gap: 6 }}>
                      <button
                        type="button"
                        className="btn btn-sm btn-primary"
                        onClick={() => void handleSmartDownload(dl.nodeId, dl.mountId, dl.relPath, dl.name)}
                      >
                        Resume
                      </button>
                      <button
                        type="button"
                        className="btn btn-sm"
                        onClick={() => void clearResumableDownload(dl.key)}
                      >
                        Discard
                      </button>
                    </div>
                  </div>
                );
              })}
            </div>
          )}

          {uploadStatus && <div className="status-banner">{uploadStatus}</div>}
          {error && (
            <div className="alert-error" role="alert">
              {error}
            </div>
          )}

          {/* File Listing */}
          {loading ? (
            <div className="mounts-empty">Loading files…</div>
          ) : !selectedMount ? (
            <div className="mounts-empty">Add a shared folder in Contribution settings to browse files here.</div>
          ) : entries.length === 0 ? (
            <div className="mounts-empty">
              {selectedNode.status === "online"
                ? "This folder is empty. Drop files here or click Upload."
                : "Unavailable while this machine is offline."}
            </div>
          ) : viewStyle === "table" ? (
            <div tabIndex={0} role="region" aria-label="Files" className="vfs-table-wrapper">
              <table className="vfs-table">
                <thead>
                  <tr>
                    <th>Name</th>
                    <th>Tags</th>
                    <th className="num-col">Size</th>
                    <th>Modified</th>
                    <th className="num-col">Action</th>
                  </tr>
                </thead>
                <tbody>
                  {visibleTableRows.length === 0 ? (
                    <tr>
                      <td colSpan={5} style={{ textAlign: "center", color: "var(--text-muted)", padding: "24px 12px" }}>
                        No files match your current filter.
                      </td>
                    </tr>
                  ) : (
                    visibleTableRows.map(({ entry, depth }) => {
                      const isExpanded = Boolean(expandedDirs[entry.path]);
                      const isExpanding = Boolean(expandingPaths[entry.path]);
                      return (
                        <tr
                          key={entry.path}
                          className="vfs-row"
                          onClick={() => {
                            if (entry.is_dir) {
                              setRelPath(entry.path);
                            } else {
                              openPreview(entry);
                            }
                          }}
                        >
                          <th>
                            <div className="file-cell" style={{ paddingInlineStart: depth * 20 }}>
                              <input
                                type="checkbox"
                                aria-label={`Select ${entry.name}`}
                                checked={selectedPaths.includes(entry.path)}
                                onClick={(e) => e.stopPropagation()}
                                onChange={() => toggleSelectPath(entry.path)}
                                style={{ marginRight: 6 }}
                              />
                              {entry.is_dir ? (
                                <button
                                  type="button"
                                  className="tree-toggle"
                                  aria-expanded={isExpanded}
                                  aria-label={isExpanded ? `Collapse ${entry.name}` : `Expand ${entry.name}`}
                                  onClick={(e) => {
                                    e.stopPropagation();
                                    void toggleExpandDir(entry.path);
                                  }}
                                >
                                  {isExpanded ? (
                                    <ChevronDown size={14} strokeWidth={1.7} />
                                  ) : (
                                    <ChevronRight size={14} strokeWidth={1.7} />
                                  )}
                                </button>
                              ) : (
                                <span className="tree-spacer" aria-hidden="true" />
                              )}
                              <EntryIcon entry={entry} />
                              <span className="file-name">
                                {entry.name}
                                {isExpanding && <span className="node-sub" style={{ marginLeft: 8 }}>…</span>}
                              </span>
                            </div>
                          </th>
                          <td>
                            <div className="tag-inline-list">
                              {(entry.tags ?? []).map((t) => (
                                <span key={t} className="tag-chip mono">
                                  #{t}
                                </span>
                              ))}
                            </div>
                          </td>
                          <td className="num-col mono">{entry.is_dir ? "—" : formatBytes(entry.size)}</td>
                          <td className="mono">{formatDate(entry.mod_time)}</td>
                          <td className="num-col" onClick={(e) => e.stopPropagation()}>
                            <div className="row-actions">
                              {!entry.is_dir &&
                                (isOwnHost ? (
                                  <button
                                    type="button"
                                    className="icon-btn"
                                    title="Show on this PC"
                                    aria-label={`Show ${entry.name} in local file manager`}
                                    onClick={() => handleRevealLocal(selectedNode.id, selectedMount.id, entry.path)}
                                  >
                                    <FolderOpen size={14} strokeWidth={1.6} />
                                  </button>
                                ) : (
                                  <button
                                    type="button"
                                    className="icon-btn"
                                    title="Download (P2P direct)"
                                    aria-label={`Download ${entry.name}`}
                                    onClick={() => handleSmartDownload(selectedNode.id, selectedMount.id, entry.path, entry.name)}
                                  >
                                    <Download size={14} strokeWidth={1.6} />
                                  </button>
                                ))}
                              {entry.is_dir && selectedNode.status === "online" && (
                                <button
                                  type="button"
                                  className="icon-btn"
                                  title="Download folder as .zip (P2P direct)"
                                  aria-label={`Download ${entry.name} as zip`}
                                  onClick={() => handleSmartZip(selectedNode.id, selectedMount.id, entry.path, entry.name)}
                                >
                                  <Download size={14} strokeWidth={1.6} />
                                </button>
                              )}
                              {selectedNode.is_owner && selectedNode.status === "online" && (
                                <button
                                  type="button"
                                  className="icon-btn"
                                  title="Rename or move"
                                  aria-label={`Rename ${entry.name}`}
                                  onClick={() => void handleRenameEntry(entry)}
                                >
                                  <Pencil size={14} strokeWidth={1.6} />
                                </button>
                              )}
                              <button
                                type="button"
                                className="btn btn-sm"
                                onClick={() => setInspected(entry)}
                              >
                                Details
                              </button>
                            </div>
                          </td>
                        </tr>
                      );
                    })
                  )}
                </tbody>
              </table>
            </div>
          ) : viewStyle === "grid" ? (
            <div className="vfs-grid">
              {visibleGridEntries.length === 0 ? (
                <div className="mounts-empty" style={{ gridColumn: "1 / -1" }}>
                  No files match your current filter.
                </div>
              ) : (
                visibleGridEntries.map((entry) => (
                  <button
                    key={entry.path}
                    type="button"
                    className="vfs-grid-card"
                    onClick={() => {
                      if (entry.is_dir) setRelPath(entry.path);
                      else openPreview(entry);
                    }}
                  >
                    <div className="grid-thumb-box">
                      <ThumbPreview nodeId={selectedNode.id} mountId={selectedMount.id} entry={entry} />
                    </div>
                    <div className="grid-card-name">{entry.name}</div>
                    <div className="grid-card-meta mono">
                      {entry.is_dir ? "Folder" : formatBytes(entry.size)}
                    </div>
                  </button>
                ))
              )}
            </div>
          ) : (
            <div className="gallery-split">
              <div className="gallery-rail" role="listbox" aria-label="Files">
                {visibleGridEntries.map((entry) => (
                  <button
                    key={entry.path}
                    type="button"
                    role="option"
                    aria-selected={galleryEntry?.path === entry.path}
                    className="gallery-item"
                    onClick={() => {
                      if (entry.is_dir) setRelPath(entry.path);
                      else setGallerySelectedPath(entry.path);
                    }}
                  >
                    <div className="gallery-mini-thumb">
                      <ThumbPreview nodeId={selectedNode.id} mountId={selectedMount.id} entry={entry} />
                    </div>
                    <div className="gallery-item-text">
                      <div className="file-name">{entry.name}</div>
                      <div className="grid-card-meta mono">
                        {entry.is_dir ? "Folder" : formatBytes(entry.size)}
                      </div>
                    </div>
                  </button>
                ))}
              </div>

              <div className="gallery-preview-pane">
                {galleryEntry && !galleryEntry.is_dir ? (
                  <>
                    <div className="gallery-preview-bar">
                      <div style={{ minWidth: 0 }}>
                        <div className="file-name" style={{ fontWeight: 500 }}>
                          {galleryEntry.name}
                        </div>
                        <div className="grid-card-meta mono">
                          {formatBytes(galleryEntry.size)} · {galleryEntry.mime_type}
                        </div>
                      </div>
                      <div className="row-actions">
                        <button type="button" className="btn btn-sm" onClick={() => setInspected(galleryEntry)}>
                          Details
                        </button>
                        <button
                          type="button"
                          className="icon-btn"
                          title="Fullscreen preview"
                          aria-label="Fullscreen preview"
                          onClick={() => openPreview(galleryEntry)}
                        >
                          <Maximize2 size={14} strokeWidth={1.6} />
                        </button>
                        {isOwnHost ? (
                          <button
                            type="button"
                            className="btn btn-sm"
                            onClick={() => handleRevealLocal(selectedNode.id, selectedMount.id, galleryEntry.path)}
                          >
                            <FolderOpen size={13} strokeWidth={1.6} />
                            <span>Show on PC</span>
                          </button>
                        ) : (
                          <button
                            type="button"
                            className="btn btn-primary btn-sm"
                            onClick={() =>
                              handleSmartDownload(selectedNode.id, selectedMount.id, galleryEntry.path, galleryEntry.name)
                            }
                          >
                            <Download size={13} strokeWidth={1.6} />
                            <span>Download</span>
                          </button>
                        )}
                      </div>
                    </div>
                    <div
                      className={`gallery-preview-body ${
                        galleryEntry.mime_type.startsWith("image/") || galleryEntry.mime_type.startsWith("video/")
                          ? ""
                          : "light-stage"
                      }`}
                    >
                      {galleryEntry.mime_type.startsWith("image/") ? (
                        <img
                          key={galleryEntry.path}
                          src={api.fileUrl(selectedNode.id, selectedMount.id, galleryEntry.path)}
                          alt={galleryEntry.name}
                        />
                      ) : galleryEntry.mime_type.startsWith("video/") ? (
                        <video
                          key={galleryEntry.path}
                          controls
                          playsInline
                          width={960}
                          height={540}
                          preload="metadata"
                          poster={api.thumbUrl(selectedNode.id, selectedMount.id, galleryEntry.path)}
                          src={api.fileUrl(selectedNode.id, selectedMount.id, galleryEntry.path)}
                        />
                      ) : galleryEntry.mime_type.startsWith("audio/") ? (
                        <div className="preview-audio-stage">
                          <audio
                            key={galleryEntry.path}
                            controls
                            preload="metadata"
                            src={api.fileUrl(selectedNode.id, selectedMount.id, galleryEntry.path)}
                            className="preview-audio"
                          />
                        </div>
                      ) : galleryEntry.mime_type === "application/pdf" ? (
                        <iframe
                          key={galleryEntry.path}
                          src={api.fileUrl(selectedNode.id, selectedMount.id, galleryEntry.path)}
                          title={galleryEntry.name}
                          className="preview-pdf"
                        />
                      ) : (
                        <div className="preview-fallback">
                          <EntryIcon entry={galleryEntry} size={28} />
                          <div className="empty-title">{galleryEntry.name}</div>
                          <button type="button" className="btn" onClick={() => openPreview(galleryEntry)}>
                            <Eye size={14} strokeWidth={1.6} />
                            <span>Open preview</span>
                          </button>
                        </div>
                      )}
                    </div>
                  </>
                ) : (
                  <div className="preview-fallback" style={{ gridRow: "1 / -1" }}>
                    <Folder size={24} strokeWidth={1.4} className="muted-icon" />
                    <div className="empty-copy">Select a file on the left to preview it inline.</div>
                  </div>
                )}
              </div>
            </div>
          )}
        </section>
      )}

      {/* Shared Trees Mode */}
      {mode === "trees" && (
        <section className="card">
          {tags.length === 0 ? (
            <div className="empty-card" style={{ padding: "32px 16px" }}>
              <Hash size={22} strokeWidth={1.4} className="muted-icon" />
              <div className="empty-title">No shared trees yet</div>
              <p className="empty-copy">
                Tag any file or folder from the Inspector drawer to compose a multi-machine shared tree without copying
                files.
              </p>
              <button type="button" className="btn btn-primary" onClick={() => setNewTreeOpen(true)}>
                <Plus size={14} strokeWidth={1.6} />
                <span>Create first shared tree</span>
              </button>
            </div>
          ) : (
            <div className="section-stack">
              <div className="section-head" style={{ marginBottom: 0 }}>
                <div>
                  <h2 className="section-title">#{selectedTreeSlug}</h2>
                  <p className="page-sub">
                    Composed across contributor machines — every item shows its physical host and live traffic.
                  </p>
                </div>
              </div>

              {treeItems.length === 0 ? (
                <div className="mounts-empty">
                  No files or folders tagged with <span className="mono">#{selectedTreeSlug}</span> yet. Open any
                  item&apos;s Details in Nodes view to add this tag.
                </div>
              ) : (
                <div tabIndex={0} role="region" aria-label="Shared tree items" className="vfs-table-wrapper">
                  <table className="vfs-table">
                    <thead>
                      <tr>
                        <th>Item</th>
                        <th>Hosted on</th>
                        <th>Live traffic</th>
                        <th className="num-col">Action</th>
                      </tr>
                    </thead>
                    <tbody>
                      {treeItems.map((it) => {
                        const isOffline = it.node_status === "offline";
                        return (
                          <tr
                            key={it.id}
                            className={`vfs-row ${isOffline ? "offline-row" : ""}`}
                            onClick={() => openTreeItem(it)}
                          >
                            <th>
                              <div className="file-cell">
                                {it.is_dir ? (
                                  <Folder size={16} strokeWidth={1.5} className="icon-folder" />
                                ) : (
                                  <FileIcon size={16} strokeWidth={1.5} />
                                )}
                                <div>
                                  <div className="file-name">{it.rel_path.split("/").pop() || it.mount_name}</div>
                                  <div className="mount-path mono">
                                    {it.mount_name}/{it.rel_path}
                                  </div>
                                </div>
                              </div>
                            </th>
                            <td>
                              <span className="node-title-row">
                                <span className={`dot ${isOffline ? "offline" : it.node_status === "paused" ? "paused" : ""}`} />
                                <span>{it.node_name}</span>
                              </span>
                            </td>
                            <td>
                              <div className="io-pair">
                                <span className="io-up">↑ {formatMbps(it.traffic.upload_bps)}</span>
                                <span className="io-down">↓ {formatMbps(it.traffic.download_bps)}</span>
                              </div>
                            </td>
                            <td className="num-col" onClick={(e) => e.stopPropagation()}>
                              {it.is_dir ? (
                                <button type="button" className="btn btn-sm" onClick={() => openTreeItem(it)}>
                                  Open
                                </button>
                              ) : (
                                <a
                                  className="btn btn-sm"
                                  aria-disabled={isOffline}
                                  href={isOffline ? undefined : api.fileUrl(it.node_id, it.mount_id, it.rel_path, true)}
                                >
                                  <Download size={13} strokeWidth={1.6} />
                                  <span>Download</span>
                                </a>
                              )}
                            </td>
                          </tr>
                        );
                      })}
                    </tbody>
                  </table>
                </div>
              )}
            </div>
          )}
        </section>
      )}

      {/* On-Demand Slide-Over Inspector Drawer */}
      <dialog
        ref={drawerRef}
        className="inspector-drawer"
        aria-labelledby={drawerTitleId}
        onClose={() => setInspected(null)}
        onClick={(e) => {
          if (e.target === drawerRef.current) drawerRef.current?.close();
        }}
      >
        {inspected && selectedNode && selectedMount && (
          <div className="inspector-sheet">
            <div className="inspector-head">
              <div className="node-title-row">
                <EntryIcon entry={inspected} size={18} />
                <h2 id={drawerTitleId} className="modal-title" style={{ wordBreak: "break-word" }}>
                  {inspected.name}
                </h2>
              </div>
              <button
                type="button"
                className="icon-btn"
                aria-label="Close inspector"
                autoFocus
                onClick={() => drawerRef.current?.close()}
              >
                <X size={16} strokeWidth={1.6} />
              </button>
            </div>

            <div className="inspector-body">
              {!inspected.is_dir &&
                (inspected.mime_type.startsWith("image/") || inspected.mime_type.startsWith("video/")) && (
                  <div className="inspector-thumb" onClick={() => { drawerRef.current?.close(); openPreview(inspected); }}>
                    <ThumbPreview nodeId={selectedNode.id} mountId={selectedMount.id} entry={inspected} />
                  </div>
                )}

              <dl className="meta-list">
                <div>
                  <dt>Hosted on</dt>
                  <dd className="node-title-row">
                    <span className={`dot ${selectedNode.status === "offline" ? "offline" : ""}`} />
                    <span>{selectedNode.display_name}</span>
                  </dd>
                </div>
                <div>
                  <dt>Folder</dt>
                  <dd className="mono">{selectedMount.name}/{inspected.path}</dd>
                </div>
                <div>
                  <dt>Type</dt>
                  <dd className="mono">{inspected.is_dir ? "Folder" : inspected.mime_type}</dd>
                </div>
                {!inspected.is_dir && (
                  <div>
                    <dt>Size</dt>
                    <dd className="mono">{formatBytes(inspected.size)}</dd>
                  </div>
                )}
                <div>
                  <dt>Modified</dt>
                  <dd className="mono">{formatDate(inspected.mod_time)}</dd>
                </div>
              </dl>

              <div className="inspector-section">
                <div className="mounts-head">Shared tree tags</div>
                <div className="member-chips">
                  {(inspected.tags ?? []).length === 0 ? (
                    <span className="node-sub">Not in any shared tree yet.</span>
                  ) : (
                    (inspected.tags ?? []).map((slug) => (
                      <span key={slug} className="member-chip mono">
                        #{slug}
                        <button
                          type="button"
                          aria-label={`Remove tag ${slug}`}
                          onClick={() => handleDetachTag(slug)}
                        >
                          ×
                        </button>
                      </span>
                    ))
                  )}
                </div>
                <form className="tag-add-form" onSubmit={handleAttachTag}>
                  <input
                    type="text"
                    placeholder="Add tag (e.g. clips-2026)"
                    value={tagInput}
                    onChange={(e) => setTagInput(e.target.value)}
                  />
                  <button type="submit" className="btn">
                    Add
                  </button>
                </form>
              </div>
            </div>

            <div className="inspector-foot">
              {inspectedShare ? (
                <button
                  type="button"
                  className="btn"
                  onClick={async () => {
                    try {
                      await navigator.clipboard.writeText(inspectedShare.url);
                      setCopiedInspectedLink(true);
                      setTimeout(() => setCopiedInspectedLink(false), 1800);
                    } catch {}
                  }}
                >
                  {copiedInspectedLink ? <Check size={14} /> : <Link2 size={14} strokeWidth={1.6} />}
                  <span>{copiedInspectedLink ? "Copied" : "Copy share link"}</span>
                </button>
              ) : (
                <button
                  type="button"
                  className="btn"
                  onClick={() => {
                    const entry = inspected;
                    drawerRef.current?.close();
                    openShareForEntry(selectedNode, selectedMount.id, entry);
                  }}
                >
                  <Link2 size={14} strokeWidth={1.6} />
                  <span>{inspected.is_dir ? "Share ZIP link" : "Create share link"}</span>
                </button>
              )}
              {!inspected.is_dir && (
                <>
                  <button
                    type="button"
                    className="btn"
                    onClick={() => {
                      const targetEntry = inspected;
                      drawerRef.current?.close();
                      openPreview(targetEntry);
                    }}
                  >
                    <Eye size={14} strokeWidth={1.6} />
                    <span>Preview</span>
                  </button>
                  {isOwnHost ? (
                    <button
                      type="button"
                      className="btn"
                      onClick={() => handleRevealLocal(selectedNode.id, selectedMount.id, inspected.path)}
                    >
                      <FolderOpen size={14} strokeWidth={1.6} />
                      <span>Show on PC</span>
                    </button>
                  ) : (
                    <button
                      type="button"
                      className="btn btn-primary"
                      onClick={() => handleSmartDownload(selectedNode.id, selectedMount.id, inspected.path, inspected.name)}
                    >
                      <Download size={14} strokeWidth={1.6} />
                      <span>Download</span>
                    </button>
                  )}
                </>
              )}
              {selectedNode.is_owner && (
                <button type="button" className="btn btn-danger" onClick={() => handleDeleteEntry(inspected)}>
                  <Trash2 size={14} strokeWidth={1.6} />
                  <span>Delete</span>
                </button>
              )}
            </div>
          </div>
        )}
      </dialog>

      {/* New Folder Modal */}
      <ModalDialog open={mkdirOpen} title="Create folder" onClose={() => setMkdirOpen(false)}>
        <form className="form-stack" onSubmit={handleMkdir}>
          <div className="field">
            <label htmlFor="new-folder-name">Folder name</label>
            <input
              id="new-folder-name"
              type="text"
              required
              autoFocus
              value={newFolderName}
              onChange={(e) => setNewFolderName(e.target.value)}
              placeholder="e.g. 2026-clips"
            />
          </div>
          <div className="modal-actions">
            <button type="button" className="btn" onClick={() => setMkdirOpen(false)}>
              Cancel
            </button>
            <button type="submit" className="btn btn-primary">
              Create folder
            </button>
          </div>
        </form>
      </ModalDialog>

      {/* New Shared Tree Modal */}
      <ModalDialog open={newTreeOpen} title="Create shared tree" onClose={() => setNewTreeOpen(false)}>
        <form className="form-stack" onSubmit={handleCreateTree}>
          <div className="field">
            <label htmlFor="new-tree-slug">Tag name</label>
            <span className="field-hint">Files and folders with this tag appear together across all nodes.</span>
            <input
              id="new-tree-slug"
              type="text"
              required
              autoFocus
              value={newTreeSlug}
              onChange={(e) => setNewTreeSlug(e.target.value)}
              placeholder="e.g. game-clips"
            />
          </div>
          <div className="field">
            <label htmlFor="new-tree-desc">Description (optional)</label>
            <input
              id="new-tree-desc"
              type="text"
              value={newTreeDesc}
              onChange={(e) => setNewTreeDesc(e.target.value)}
              placeholder="What belongs in this tree?"
            />
          </div>
          <div className="modal-actions">
            <button type="button" className="btn" onClick={() => setNewTreeOpen(false)}>
              Cancel
            </button>
            <button type="submit" className="btn btn-primary">
              Create tree
            </button>
          </div>
        </form>
      </ModalDialog>
      {/* Rich On-Demand File Previewer */}
      <FilePreviewModal
        target={previewTarget}
        isOwnHost={Boolean(
          previewTarget && nodes.find((n) => n.id === previewTarget.nodeId)?.owner_id === currentUser.id,
        )}
        hasPrev={previewIndex > 0}
        hasNext={previewIndex >= 0 && previewIndex < fileEntries.length - 1}
        existingShareUrl={previewShare?.url}
        onPrev={previewIndex > 0 ? () => openPreview(fileEntries[previewIndex - 1]) : undefined}
        onNext={
          previewIndex >= 0 && previewIndex < fileEntries.length - 1
            ? () => openPreview(fileEntries[previewIndex + 1])
            : undefined
        }
        onRevealLocal={
          previewTarget
            ? () => void handleRevealLocal(previewTarget.nodeId, previewTarget.mountId, previewTarget.entry.path)
            : undefined
        }
        onInspect={
          previewTarget && selectedMount && previewTarget.mountId === selectedMount.id
            ? () => {
                const entry = previewTarget.entry;
                setPreviewTarget(null);
                setInspected(entry);
              }
            : undefined
        }
        onShare={
          previewTarget
            ? () => {
                const t = previewTarget;
                setPreviewTarget(null);
                setShareTarget({
                  nodeId: t.nodeId,
                  nodeName: t.nodeName,
                  mountId: t.mountId,
                  relPath: t.entry.path,
                  isDir: t.entry.is_dir,
                  title: t.entry.name,
                  mimeType: t.entry.mime_type,
                  sizeBytes: t.entry.size,
                });
              }
            : undefined
        }
        onClose={() => setPreviewTarget(null)}
      />

      {/* Create Share Link Modal */}
      <CreateShareModal
        target={shareTarget}
        onCreated={() => void onSharesChanged()}
        onClose={() => setShareTarget(null)}
      />

      {/* Add Ourchive Hub Modal */}
      <ModalDialog open={addHubOpen} title="Add Ourchive Hub" onClose={() => setAddHubOpen(false)}>
        <form className="form-stack" onSubmit={handleSaveHub}>
          <div className="field">
            <label htmlFor="hub-url-input">Hub URL</label>
            <span className="field-hint">Each Hub stays strictly segregated with its own login and mounts.</span>
            <input
              id="hub-url-input"
              type="text"
              required
              className="mono"
              placeholder="https://archive.friends.org"
              value={newHubUrl}
              onChange={(e) => setNewHubUrl(e.target.value)}
            />
          </div>
          <div className="field">
            <label htmlFor="hub-name-input">Label (optional)</label>
            <input
              id="hub-name-input"
              type="text"
              placeholder="e.g. Friends Archive"
              value={newHubName}
              onChange={(e) => setNewHubName(e.target.value)}
            />
          </div>
          <div className="modal-actions">
            <button type="button" className="btn" onClick={() => setAddHubOpen(false)}>
              Cancel
            </button>
            <button type="submit" className="btn btn-primary">
              Add hub
            </button>
          </div>
        </form>
      </ModalDialog>

      {/* Opt-In Parity Sync Subscription Modal */}
      <ModalDialog open={syncModalOpen} title="Opt in to local mirroring" onClose={() => setSyncModalOpen(false)}>
        <form className="form-stack" onSubmit={handleSubscribeSync}>
          <p className="page-sub" style={{ marginTop: 0 }}>
            Mirror <span className="mono">{selectedMount?.name}</span> onto one of your connected machines for shared
            asset parity and drive-failure redundancy. Either you or the contributor can disable this at any time.
          </p>
          {myNodes.length === 0 ? (
            <div className="alert-error">Connect one of your own machines in Contribution settings first.</div>
          ) : (
            <>
              <div className="field">
                <label htmlFor="sync-node">Destination machine</label>
                <select
                  id="sync-node"
                  value={syncTargetNodeId}
                  onChange={(e) => setSyncTargetNodeId(e.target.value)}
                  required
                >
                  {myNodes.map((n) => (
                    <option key={n.id} value={n.id}>
                      {n.display_name} ({n.status})
                    </option>
                  ))}
                </select>
              </div>
              <div className="field">
                <label htmlFor="sync-dest">Local destination folder</label>
                <input
                  id="sync-dest"
                  type="text"
                  className="mono"
                  required
                  value={syncTargetPath}
                  onChange={(e) => setSyncTargetPath(e.target.value)}
                />
              </div>
            </>
          )}
          <div className="modal-actions">
            <button type="button" className="btn" onClick={() => setSyncModalOpen(false)}>
              Cancel
            </button>
            <button type="submit" className="btn btn-primary" disabled={myNodes.length === 0}>
              Enable parity sync
            </button>
          </div>
        </form>
      </ModalDialog>

      <ModalDialog open={globalSearchOpen} title="Global archive search" onClose={() => setGlobalSearchOpen(false)}>
        <form className="form-stack" onSubmit={handleGlobalSearch}>
          <div style={{ display: "flex", gap: 8 }}>
            <input
              type="search"
              placeholder="Search filenames across all online machines…"
              value={globalQuery}
              onChange={(e) => setGlobalQuery(e.target.value)}
              style={{ flex: 1 }}
              autoFocus
            />
            <button type="submit" className="btn btn-primary" disabled={globalSearching}>
              {globalSearching ? "Searching…" : "Search"}
            </button>
          </div>
          {globalHits.length === 0 ? (
            <p className="inline-muted" style={{ margin: 0 }}>
              {globalQuery.trim() && !globalSearching ? "No matching files found on online machines." : "Type a filename fragment to search all accessible folders."}
            </p>
          ) : (
            <div style={{ display: "flex", flexDirection: "column", gap: 6, maxHeight: 360, overflowY: "auto" }}>
              {globalHits.map((hit, idx) => (
                <button
                  key={`${hit.node_id}-${hit.mount_id}-${hit.path}-${idx}`}
                  type="button"
                  className="btn"
                  style={{ justifyContent: "space-between", height: "auto", padding: "8px 10px", textAlign: "left" }}
                  onClick={() => {
                    setMode("nodes");
                    setSelectedNodeId(hit.node_id);
                    setSelectedMountId(hit.mount_id);
                    const parts = hit.path.split("/");
                    setRelPath(hit.is_dir ? hit.path : parts.slice(0, -1).join("/"));
                    setGlobalSearchOpen(false);
                  }}
                >
                  <div style={{ minWidth: 0 }}>
                    <div style={{ fontWeight: 500, overflow: "hidden", textOverflow: "ellipsis" }}>{hit.name}</div>
                    <div className="inline-muted mono" style={{ fontSize: 11 }}>
                      {hit.node_name} / {hit.mount_name} / {hit.path}
                    </div>
                  </div>
                  <span className="inline-muted mono" style={{ fontSize: 12 }}>
                    {hit.is_dir ? "Folder" : formatBytes(hit.size)}
                  </span>
                </button>
              ))}
            </div>
          )}
        </form>
      </ModalDialog>
    </div>
  );
}

function ThumbPreview({ nodeId, mountId, entry }: { nodeId: string; mountId: string; entry: FileEntry }) {
  const [failed, setFailed] = useState(false);
  const hasMediaThumb =
    !entry.is_dir && (entry.mime_type.startsWith("image/") || entry.mime_type.startsWith("video/"));

  if (!hasMediaThumb || failed) {
    return <EntryIcon entry={entry} size={24} />;
  }

  return (
    <img
      src={api.thumbUrl(nodeId, mountId, entry.path)}
      alt={entry.name}
      loading="lazy"
      width={240}
      height={150}
      className="thumb-img"
      onError={() => setFailed(true)}
    />
  );
}

function guessMimeFromName(name: string): string {
  const ext = name.slice(name.lastIndexOf(".")).toLowerCase();
  if ([".mp4", ".m4v"].includes(ext)) return "video/mp4";
  if (ext === ".webm") return "video/webm";
  if (ext === ".mkv") return "video/x-matroska";
  if (ext === ".mov") return "video/quicktime";
  if (ext === ".mp3") return "audio/mpeg";
  if (ext === ".flac") return "audio/flac";
  if (ext === ".wav") return "audio/wav";
  if ([".ogg", ".opus"].includes(ext)) return "audio/ogg";
  if (ext === ".png") return "image/png";
  if ([".jpg", ".jpeg"].includes(ext)) return "image/jpeg";
  if (ext === ".webp") return "image/webp";
  if (ext === ".gif") return "image/gif";
  if (ext === ".svg") return "image/svg+xml";
  if (ext === ".pdf") return "application/pdf";
  if (ext === ".md") return "text/markdown";
  if (ext === ".json") return "application/json";
  return "application/octet-stream";
}

function EntryIcon({ entry, size = 16 }: { entry: FileEntry; size?: number }) {
  if (entry.is_dir) return <Folder size={size} strokeWidth={1.5} className="icon-folder" />;
  if (entry.mime_type.startsWith("video/")) return <FileVideo size={size} strokeWidth={1.5} />;
  if (entry.mime_type.startsWith("audio/")) return <FileAudio size={size} strokeWidth={1.5} />;
  if (entry.mime_type.startsWith("image/")) return <FileImage size={size} strokeWidth={1.5} />;
  if (entry.mime_type.startsWith("text/") || entry.mime_type === "application/pdf") {
    return <FileText size={size} strokeWidth={1.5} />;
  }
  return <FileIcon size={size} strokeWidth={1.5} />;
}

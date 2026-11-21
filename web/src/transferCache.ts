/**
 * transferCache.ts — Crash-resilient persistent download cache backed by
 * Origin Private File System (OPFS) with automatic IndexedDB fallback.
 * Survives dropped Wi-Fi, closed tabs, browser crashes, and PC power loss so
 * partial P2P downloads can resume from the exact last saved byte with ETag validation.
 */

export interface ResumableDownloadMeta {
  key: string;
  nodeId: string;
  mountId: string;
  relPath: string;
  name: string;
  mimeType: string;
  totalSize: number;
  receivedBytes: number;
  etag: string;
  updatedAt: number;
}

const META_STORAGE_KEY = "ourchive-resumable-downloads-v1";
const IDB_NAME = "ourchive-transfer-cache";
const IDB_STORE = "chunks";

export function makeDownloadKey(nodeId: string, mountId: string, relPath: string): string {
  return `${nodeId}::${mountId}::${relPath}`;
}

function safeFileName(key: string): string {
  let hash = 2166136261;
  for (let i = 0; i < key.length; i++) {
    hash ^= key.charCodeAt(i);
    hash = Math.imul(hash, 16777619);
  }
  return `dl_${(hash >>> 0).toString(16)}_${key.length}.part`;
}

function readMetaMap(): Record<string, ResumableDownloadMeta> {
  try {
    const raw = localStorage.getItem(META_STORAGE_KEY);
    if (!raw) return {};
    return JSON.parse(raw) as Record<string, ResumableDownloadMeta>;
  } catch {
    return {};
  }
}

function writeMetaMap(map: Record<string, ResumableDownloadMeta>) {
  try {
    localStorage.setItem(META_STORAGE_KEY, JSON.stringify(map));
    window.dispatchEvent(new Event("ourchive-transfers-changed"));
  } catch {}
}

export function listResumableDownloads(): ResumableDownloadMeta[] {
  return Object.values(readMetaMap()).sort((a, b) => b.updatedAt - a.updatedAt);
}

export function getResumableDownload(key: string): ResumableDownloadMeta | null {
  return readMetaMap()[key] ?? null;
}

export function saveResumableMeta(meta: ResumableDownloadMeta) {
  const map = readMetaMap();
  map[meta.key] = { ...meta, updatedAt: Date.now() };
  writeMetaMap(map);
}

// --- IndexedDB fallback helpers ---
function openIDB(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(IDB_NAME, 1);
    req.onupgradeneeded = () => {
      const db = req.result;
      if (!db.objectStoreNames.contains(IDB_STORE)) {
        db.createObjectStore(IDB_STORE);
      }
    };
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error || new Error("IndexedDB unavailable"));
  });
}

async function idbAppend(key: string, offset: number, bytes: Uint8Array): Promise<void> {
  const db = await openIDB();
  await new Promise<void>((resolve, reject) => {
    const tx = db.transaction(IDB_STORE, "readwrite");
    tx.objectStore(IDB_STORE).put(bytes, `${key}::${String(offset).padStart(16, "0")}`);
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error);
  });
  db.close();
}

async function idbReadAll(key: string, mimeType: string): Promise<Blob> {
  const db = await openIDB();
  const prefix = `${key}::`;
  const parts: BlobPart[] = [];
  await new Promise<void>((resolve, reject) => {
    const tx = db.transaction(IDB_STORE, "readonly");
    const store = tx.objectStore(IDB_STORE);
    const req = store.openCursor();
    req.onsuccess = () => {
      const cursor = req.result;
      if (!cursor) return;
      if (typeof cursor.key === "string" && cursor.key.startsWith(prefix)) {
        parts.push(cursor.value as Uint8Array<ArrayBuffer>);
      }
      cursor.continue();
    };
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error);
  });
  db.close();
  return new Blob(parts, { type: mimeType });
}

async function idbClear(key: string): Promise<void> {
  const db = await openIDB();
  const prefix = `${key}::`;
  await new Promise<void>((resolve, reject) => {
    const tx = db.transaction(IDB_STORE, "readwrite");
    const store = tx.objectStore(IDB_STORE);
    const req = store.openCursor();
    req.onsuccess = () => {
      const cursor = req.result;
      if (!cursor) return;
      if (typeof cursor.key === "string" && cursor.key.startsWith(prefix)) {
        cursor.delete();
      }
      cursor.continue();
    };
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error);
  });
  db.close();
}

async function getOPFSHandle(key: string, create: boolean): Promise<FileSystemFileHandle | null> {
  try {
    if (typeof navigator === "undefined" || !navigator.storage?.getDirectory) return null;
    const root = await navigator.storage.getDirectory();
    const dir = await root.getDirectoryHandle("ourchive-parts", { create: true });
    return await dir.getFileHandle(safeFileName(key), { create });
  } catch {
    return null;
  }
}

/**
 * PersistentPartWriter appends incoming chunks directly to OPFS (or IndexedDB fallback)
 * so partial downloads survive power loss or closed tabs.
 */
export class PersistentPartWriter {
  private key: string;
  private opfsHandle: FileSystemFileHandle | null = null;
  private useOPFS = false;
  private writeOffset = 0;

  constructor(key: string, initialOffset: number) {
    this.key = key;
    this.writeOffset = initialOffset;
  }

  async init(reset: boolean): Promise<void> {
    if (reset) {
      await clearResumableDownload(this.key);
      this.writeOffset = 0;
    }
    const handle = await getOPFSHandle(this.key, true);
    if (handle && typeof handle.createWritable === "function") {
      this.opfsHandle = handle;
      this.useOPFS = true;
    } else {
      this.useOPFS = false;
    }
  }

  async append(data: Uint8Array): Promise<number> {
    if (data.byteLength === 0) return this.writeOffset;
    if (this.useOPFS && this.opfsHandle) {
      const writable = await this.opfsHandle.createWritable({ keepExistingData: true });
      await writable.seek(this.writeOffset);
      await writable.write(data as unknown as BufferSource);
      await writable.close();
    } else {
      await idbAppend(this.key, this.writeOffset, data);
    }
    this.writeOffset += data.byteLength;
    return this.writeOffset;
  }

  async finalizeBlob(mimeType: string): Promise<Blob> {
    if (this.useOPFS && this.opfsHandle) {
      const file = await this.opfsHandle.getFile();
      return file.slice(0, file.size, mimeType);
    }
    return idbReadAll(this.key, mimeType);
  }
}

export async function clearResumableDownload(key: string): Promise<void> {
  const map = readMetaMap();
  if (key in map) {
    delete map[key];
    writeMetaMap(map);
  }
  try {
    if (typeof navigator !== "undefined" && navigator.storage?.getDirectory) {
      const root = await navigator.storage.getDirectory();
      const dir = await root.getDirectoryHandle("ourchive-parts", { create: false });
      await dir.removeEntry(safeFileName(key));
    }
  } catch {}
  try {
    await idbClear(key);
  } catch {}
}

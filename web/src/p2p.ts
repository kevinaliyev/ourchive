/**
 * p2p.ts — Egress-safe WebRTC DataChannel transport for direct PC <-> Browser
 * file downloads (with OPFS/IndexedDB crash-resilient resume & ETag validation),
 * streaming .zip folder/multi-select archives, and staged .ourchive.part uploads
 * with SHA-256 verification and automatic Hub relay fallback when permitted.
 */
import { api, PublicNetworkConfig } from "./api";
import {
  clearResumableDownload,
  getResumableDownload,
  makeDownloadKey,
  PersistentPartWriter,
  saveResumableMeta,
} from "./transferCache";

export interface P2PProgress {
  received: number;
  total: number;
  transport: "p2p" | "relay";
  resumed?: boolean;
  retrying?: number;
}

const P2P_CHUNK_SIZE = 64 * 1024;
const FLUSH_BATCH_BYTES = 1024 * 1024; // Flush to OPFS/IndexedDB every 1 MB
const MAX_AUTO_RETRIES = 3;
const MAX_SHA256_BYTES = 128 * 1024 * 1024; // Compute SHA-256 in browser for files <= 128 MB

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => window.setTimeout(resolve, ms));
}

async function computeFileSHA256Hex(file: File): Promise<string> {
  if (file.size > MAX_SHA256_BYTES || !window.crypto?.subtle) return "";
  try {
    const buf = await file.arrayBuffer();
    const digest = await window.crypto.subtle.digest("SHA-256", buf);
    return Array.from(new Uint8Array(digest))
      .map((b) => b.toString(16).padStart(2, "0"))
      .join("");
  } catch {
    return "";
  }
}

function concatUint8(parts: Uint8Array[]): Uint8Array {
  const total = parts.reduce((acc, p) => acc + p.byteLength, 0);
  const out = new Uint8Array(total);
  let pos = 0;
  for (const p of parts) {
    out.set(p, pos);
    pos += p.byteLength;
  }
  return out;
}

function waitForIceGathering(pc: RTCPeerConnection, timeoutMs = 4000): Promise<void> {
  if (pc.iceGatheringState === "complete") return Promise.resolve();
  return new Promise((resolve) => {
    const timer = window.setTimeout(() => {
      pc.removeEventListener("icegatheringstatechange", onChange);
      resolve();
    }, timeoutMs);
    const onChange = () => {
      if (pc.iceGatheringState === "complete") {
        window.clearTimeout(timer);
        pc.removeEventListener("icegatheringstatechange", onChange);
        resolve();
      }
    };
    pc.addEventListener("icegatheringstatechange", onChange);
  });
}

async function openDataChannel(
  nodeId: string,
  stunServers: string[],
): Promise<{ pc: RTCPeerConnection; dc: RTCDataChannel }> {
  const iceServers = (stunServers.length > 0 ? stunServers : ["stun:stun.l.google.com:19302"]).map((u) => ({
    urls: u,
  }));
  const pc = new RTCPeerConnection({ iceServers });
  const dc = pc.createDataChannel("ourchive-vfs", { ordered: true });
  dc.binaryType = "arraybuffer";

  const openPromise = new Promise<void>((resolve, reject) => {
    const timer = window.setTimeout(() => {
      pc.close();
      reject(new Error("WebRTC P2P connection timed out"));
    }, 10000);
    dc.onopen = () => {
      window.clearTimeout(timer);
      resolve();
    };
    dc.onerror = () => {
      window.clearTimeout(timer);
      pc.close();
      reject(new Error("WebRTC DataChannel error"));
    };
    pc.onconnectionstatechange = () => {
      if (pc.connectionState === "failed" || pc.connectionState === "disconnected") {
        window.clearTimeout(timer);
        pc.close();
        reject(new Error(`WebRTC state: ${pc.connectionState}`));
      }
    };
  });

  try {
    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
    await waitForIceGathering(pc, 4000);
    if (!pc.localDescription?.sdp) {
      pc.close();
      throw new Error("Missing local SDP");
    }
    const { sdp: answerSDP } = await api.sendP2POffer(nodeId, pc.localDescription.sdp);
    await pc.setRemoteDescription({ type: "answer", sdp: answerSDP });
    await openPromise;
    return { pc, dc };
  } catch (err) {
    pc.close();
    throw err instanceof Error ? err : new Error("P2P signaling failed");
  }
}

/**
 * Fetches a single file over WebRTC P2P with persistent OPFS/IndexedDB caching so
 * dropped connections, closed tabs, or PC power loss resume cleanly from the last saved byte.
 */
async function fetchResumableFileViaWebRTC(
  nodeId: string,
  mountId: string,
  relPath: string,
  stunServers: string[],
  onProgress?: (p: P2PProgress) => void,
): Promise<{ blob: Blob; name: string }> {
  const dlKey = makeDownloadKey(nodeId, mountId, relPath);
  const existing = getResumableDownload(dlKey);
  const { pc, dc } = await openDataChannel(nodeId, stunServers);

  return new Promise<{ blob: Blob; name: string }>((resolve, reject) => {
    let settled = false;
    let fileName = existing?.name || relPath.split("/").pop() || "download";
    let mimeType = existing?.mimeType || "application/octet-stream";
    let totalSize = existing?.totalSize || 0;
    let etag = existing?.etag || "";
    let received = existing?.receivedBytes || 0;
    let writer: PersistentPartWriter | null = null;
    let pendingChunks: Uint8Array[] = [];
    let pendingBytes = 0;
    let flushChain: Promise<void> = Promise.resolve();

    const enqueueFlush = () => {
      if (pendingBytes === 0 || !writer) return;
      const batch = concatUint8(pendingChunks);
      pendingChunks = [];
      pendingBytes = 0;
      const w = writer;
      flushChain = flushChain.then(async () => {
        const nextOffset = await w.append(batch);
        saveResumableMeta({
          key: dlKey,
          nodeId,
          mountId,
          relPath,
          name: fileName,
          mimeType,
          totalSize,
          receivedBytes: nextOffset,
          etag,
          updatedAt: Date.now(),
        });
      });
    };

    const finishErr = (err: Error) => {
      if (settled) return;
      settled = true;
      enqueueFlush();
      void flushChain.finally(() => {
        pc.close();
        reject(err);
      });
    };

    dc.onerror = () => finishErr(new Error("WebRTC DataChannel error"));
    pc.onconnectionstatechange = () => {
      if (pc.connectionState === "failed" || pc.connectionState === "disconnected") {
        finishErr(new Error(`WebRTC state: ${pc.connectionState}`));
      }
    };

    dc.onmessage = (ev) => {
      if (typeof ev.data === "string") {
        try {
          const msg = JSON.parse(ev.data);
          if (msg.ok === false) {
            finishErr(new Error(msg.error || "Agent rejected P2P transfer"));
            return;
          }
          if (msg.ok === true) {
            fileName = msg.name || fileName;
            mimeType = msg.mime_type || mimeType;
            totalSize = msg.size || totalSize;
            etag = msg.etag || etag;
            const serverOffset = typeof msg.offset === "number" ? msg.offset : 0;
            const isResumed = Boolean(msg.resumed && serverOffset > 0);
            received = serverOffset;

            const w = new PersistentPartWriter(dlKey, serverOffset);
            writer = w;
            flushChain = flushChain.then(async () => {
              await w.init(!isResumed);
              saveResumableMeta({
                key: dlKey,
                nodeId,
                mountId,
                relPath,
                name: fileName,
                mimeType,
                totalSize,
                receivedBytes: serverOffset,
                etag,
                updatedAt: Date.now(),
              });
            });
            onProgress?.({ received, total: totalSize, transport: "p2p", resumed: isResumed });
          } else if (msg.done === true) {
            if (settled) return;
            settled = true;
            enqueueFlush();
            void flushChain
              .then(async () => {
                if (!writer) throw new Error("Missing download writer");
                const blob = await writer.finalizeBlob(mimeType);
                await clearResumableDownload(dlKey);
                pc.close();
                resolve({ blob, name: fileName });
              })
              .catch((e) => {
                pc.close();
                reject(e instanceof Error ? e : new Error("Failed to finalize download"));
              });
          }
        } catch {}
        return;
      }

      if (ev.data instanceof ArrayBuffer) {
        const bytes = new Uint8Array(ev.data);
        pendingChunks.push(bytes);
        pendingBytes += bytes.byteLength;
        received += bytes.byteLength;
        onProgress?.({ received, total: totalSize, transport: "p2p" });
        if (pendingBytes >= FLUSH_BATCH_BYTES) {
          enqueueFlush();
        }
      }
    };

    dc.send(
      JSON.stringify({
        action: "get",
        mount_id: mountId,
        path: relPath,
        offset: existing?.receivedBytes ?? 0,
        if_match: existing?.etag ?? "",
      }),
    );
  });
}

async function fetchStreamViaWebRTC(
  nodeId: string,
  requestPayload: Record<string, unknown>,
  fallbackName: string,
  stunServers: string[],
  onProgress?: (p: P2PProgress) => void,
): Promise<{ blob: Blob; name: string }> {
  const { pc, dc } = await openDataChannel(nodeId, stunServers);

  return new Promise<{ blob: Blob; name: string }>((resolve, reject) => {
    let settled = false;
    const finishErr = (err: Error) => {
      if (settled) return;
      settled = true;
      pc.close();
      reject(err);
    };

    let fileName = fallbackName;
    let mimeType = "application/octet-stream";
    let totalSize = 0;
    let received = 0;
    const chunks: ArrayBuffer[] = [];

    dc.onerror = () => finishErr(new Error("WebRTC DataChannel error"));
    pc.onconnectionstatechange = () => {
      if (pc.connectionState === "failed" || pc.connectionState === "disconnected") {
        finishErr(new Error(`WebRTC state: ${pc.connectionState}`));
      }
    };

    dc.onmessage = (ev) => {
      if (typeof ev.data === "string") {
        try {
          const msg = JSON.parse(ev.data);
          if (msg.ok === false) {
            finishErr(new Error(msg.error || "Agent rejected P2P transfer"));
            return;
          }
          if (msg.ok === true) {
            fileName = msg.name || fileName;
            mimeType = msg.mime_type || mimeType;
            totalSize = msg.size || 0;
            onProgress?.({ received: 0, total: totalSize, transport: "p2p" });
          } else if (msg.done === true) {
            if (settled) return;
            settled = true;
            pc.close();
            resolve({ blob: new Blob(chunks, { type: mimeType }), name: fileName });
          }
        } catch {}
        return;
      }

      if (ev.data instanceof ArrayBuffer) {
        chunks.push(ev.data);
        received += ev.data.byteLength;
        onProgress?.({ received, total: totalSize, transport: "p2p" });
      }
    };

    dc.send(JSON.stringify(requestPayload));
  });
}

async function uploadViaWebRTC(
  nodeId: string,
  mountId: string,
  relPath: string,
  file: File,
  sha256Hex: string,
  stunServers: string[],
  onProgress?: (p: P2PProgress) => void,
): Promise<{ path: string; bytes: number }> {
  const { pc, dc } = await openDataChannel(nodeId, stunServers);

  return new Promise<{ path: string; bytes: number }>((resolve, reject) => {
    let settled = false;
    const finishErr = (err: Error) => {
      if (settled) return;
      settled = true;
      pc.close();
      reject(err);
    };

    dc.onerror = () => finishErr(new Error("WebRTC DataChannel error"));
    pc.onconnectionstatechange = () => {
      if (pc.connectionState === "failed" || pc.connectionState === "disconnected") {
        finishErr(new Error(`WebRTC state: ${pc.connectionState}`));
      }
    };

    dc.onmessage = async (ev) => {
      if (typeof ev.data !== "string") return;
      try {
        const msg = JSON.parse(ev.data);
        if (msg.ok === false) {
          finishErr(new Error(msg.error || "Agent rejected P2P upload"));
          return;
        }
        if (msg.ok === true && msg.ready === true) {
          let offset = typeof msg.offset === "number" && msg.offset > 0 && msg.offset <= file.size ? msg.offset : 0;
          const total = file.size;
          onProgress?.({ received: offset, total, transport: "p2p", resumed: Boolean(msg.resumed && offset > 0) });
          while (offset < total) {
            if (dc.readyState !== "open") {
              finishErr(new Error("WebRTC DataChannel closed during upload"));
              return;
            }
            while (dc.bufferedAmount > 1024 * 1024) {
              await sleep(12);
            }
            const slice = file.slice(offset, offset + P2P_CHUNK_SIZE);
            const buf = await slice.arrayBuffer();
            dc.send(buf);
            offset += buf.byteLength;
            onProgress?.({ received: offset, total, transport: "p2p" });
          }
          dc.send(JSON.stringify({ done: true, sha256: sha256Hex }));
          return;
        }
        if (msg.ok === true && msg.done === true) {
          if (settled) return;
          settled = true;
          pc.close();
          resolve({ path: msg.path || relPath, bytes: msg.bytes ?? file.size });
        }
      } catch (err) {
        finishErr(err instanceof Error ? err : new Error("Upload stream failed"));
      }
    };

    dc.send(JSON.stringify({ action: "put", mount_id: mountId, path: relPath, size: file.size, resume: true }));
  });
}

function triggerBlobDownload(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), 10000);
}

/**
 * Downloads a file preferring crash-resilient, resumable WebRTC P2P (zero Hub egress),
 * auto-retrying transient drops from the last saved byte, and falling back to Hub relay only if allowed.
 */
export async function downloadFileSmart(
  nodeId: string,
  mountId: string,
  relPath: string,
  netCfg: PublicNetworkConfig | null,
  onProgress?: (p: P2PProgress) => void,
): Promise<"p2p" | "relay"> {
  const canP2P = Boolean(netCfg?.p2p_enabled && typeof RTCPeerConnection !== "undefined");
  if (canP2P) {
    let lastErr: unknown = null;
    for (let attempt = 0; attempt <= MAX_AUTO_RETRIES; attempt++) {
      try {
        if (attempt > 0) {
          const saved = getResumableDownload(makeDownloadKey(nodeId, mountId, relPath));
          onProgress?.({
            received: saved?.receivedBytes ?? 0,
            total: saved?.totalSize ?? 0,
            transport: "p2p",
            retrying: attempt,
          });
          await sleep(1500 * attempt);
        }
        const { blob, name } = await fetchResumableFileViaWebRTC(
          nodeId,
          mountId,
          relPath,
          netCfg?.stun_servers ?? [],
          onProgress,
        );
        triggerBlobDownload(blob, name);
        return "p2p";
      } catch (p2pErr) {
        lastErr = p2pErr;
        const msg = p2pErr instanceof Error ? p2pErr.message : "";
        // Do not retry explicit rejection/permission errors.
        if (msg.includes("denied") || msg.includes("paused") || msg.includes("not found")) {
          break;
        }
      }
    }

    // If partial bytes were saved to disk, keep them so the user can resume later instead of restarting over relay!
    const partial = getResumableDownload(makeDownloadKey(nodeId, mountId, relPath));
    if (partial && partial.receivedBytes > 0) {
      throw new Error(
        `Connection dropped after saving ${Math.round((partial.receivedBytes / Math.max(1, partial.totalSize)) * 100)}% safely to disk. Click Resume whenever the machine is back online.`,
      );
    }

    if (netCfg && !netCfg.allow_relay_fallback) {
      throw new Error(
        `Direct P2P transfer failed (${lastErr instanceof Error ? lastErr.message : "NAT blocked"}) and Hub relay fallback is disabled by the Host Admin.`,
      );
    }
  } else if (netCfg && !netCfg.allow_relay_fallback) {
    throw new Error("Both WebRTC P2P and Hub relay fallback are disabled by the Host Admin.");
  }

  // Fallback to Hub relay streaming download (supports ETag + HTTP Range resume natively in browser).
  onProgress?.({ received: 0, total: 0, transport: "relay" });
  const url = api.fileUrl(nodeId, mountId, relPath, true);
  const a = document.createElement("a");
  a.href = url;
  document.body.appendChild(a);
  a.click();
  a.remove();
  return "relay";
}

/**
 * Downloads a folder or multi-file selection as a streaming .zip over WebRTC P2P (zero Hub egress),
 * falling back to the Hub HTTP .zip relay only when allowed by the Host Admin.
 */
export async function downloadZipSmart(
  nodeId: string,
  mountId: string,
  relPath: string,
  netCfg: PublicNetworkConfig | null,
  onProgress?: (p: P2PProgress) => void,
  paths?: string[],
): Promise<"p2p" | "relay"> {
  const canP2P = Boolean(netCfg?.p2p_enabled && typeof RTCPeerConnection !== "undefined");
  if (canP2P) {
    try {
      const fallbackName = paths && paths.length > 0 ? "selection.zip" : (relPath.split("/").pop() || "archive") + ".zip";
      const payload: Record<string, unknown> = { action: "zip", mount_id: mountId, path: relPath };
      if (paths && paths.length > 0) payload.paths = paths;
      const { blob, name } = await fetchStreamViaWebRTC(
        nodeId,
        payload,
        fallbackName,
        netCfg?.stun_servers ?? [],
        onProgress,
      );
      triggerBlobDownload(blob, name);
      return "p2p";
    } catch (p2pErr) {
      if ((paths && paths.length > 0) || (netCfg && !netCfg.allow_relay_fallback)) {
        throw new Error(
          `Direct P2P ZIP transfer failed (${p2pErr instanceof Error ? p2pErr.message : "NAT blocked"})${
            netCfg && !netCfg.allow_relay_fallback ? " and Hub relay fallback is disabled by the Host Admin." : "."
          }`,
        );
      }
    }
  } else if (netCfg && !netCfg.allow_relay_fallback) {
    throw new Error("Both WebRTC P2P and Hub relay fallback are disabled by the Host Admin.");
  }

  onProgress?.({ received: 0, total: 0, transport: "relay" });
  const url = api.zipUrl(nodeId, mountId, relPath);
  const a = document.createElement("a");
  a.href = url;
  document.body.appendChild(a);
  a.click();
  a.remove();
  return "relay";
}

/**
 * Uploads a file directly to the host Agent over WebRTC P2P (zero Hub egress) with
 * `.ourchive.part` crash-safe staging, auto-resume across drops/reboots, and SHA-256 verification.
 */
export async function uploadFileSmart(
  nodeId: string,
  mountId: string,
  relPath: string,
  file: File,
  netCfg: PublicNetworkConfig | null,
  onProgress?: (p: P2PProgress) => void,
): Promise<{ path: string; bytes: number; transport: "p2p" | "relay" }> {
  const canP2P = Boolean(netCfg?.p2p_enabled && typeof RTCPeerConnection !== "undefined");
  if (canP2P) {
    const sha256Hex = await computeFileSHA256Hex(file);
    let lastErr: unknown = null;
    for (let attempt = 0; attempt <= MAX_AUTO_RETRIES; attempt++) {
      try {
        if (attempt > 0) {
          onProgress?.({ received: 0, total: file.size, transport: "p2p", retrying: attempt });
          await sleep(1500 * attempt);
        }
        const res = await uploadViaWebRTC(
          nodeId,
          mountId,
          relPath,
          file,
          sha256Hex,
          netCfg?.stun_servers ?? [],
          onProgress,
        );
        return { ...res, transport: "p2p" };
      } catch (p2pErr) {
        lastErr = p2pErr;
        const msg = p2pErr instanceof Error ? p2pErr.message : "";
        if (msg.includes("denied") || msg.includes("paused") || msg.includes("SHA-256")) {
          break;
        }
      }
    }
    if (netCfg && !netCfg.allow_relay_fallback) {
      throw new Error(
        `Direct P2P upload interrupted (${lastErr instanceof Error ? lastErr.message : "NAT blocked"}). Partial bytes are preserved on the host — drop the same file again to resume.`,
      );
    }
  } else if (netCfg && !netCfg.allow_relay_fallback) {
    throw new Error("Both WebRTC P2P and Hub relay fallback are disabled by the Host Admin.");
  }

  onProgress?.({ received: 0, total: file.size, transport: "relay" });
  const res = await api.uploadFile(nodeId, mountId, relPath, file);
  onProgress?.({ received: file.size, total: file.size, transport: "relay" });
  return { ...res, transport: "relay" };
}

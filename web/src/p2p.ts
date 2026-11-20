import { api, PublicNetworkConfig } from "./api";

export interface P2PProgress {
  received: number;
  total: number;
  transport: "p2p" | "relay";
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

async function fetchViaWebRTC(
  nodeId: string,
  mountId: string,
  relPath: string,
  stunServers: string[],
  onProgress?: (p: P2PProgress) => void,
): Promise<{ blob: Blob; name: string }> {
  const iceServers = (stunServers.length > 0 ? stunServers : ["stun:stun.l.google.com:19302"]).map((u) => ({
    urls: u,
  }));

  const pc = new RTCPeerConnection({ iceServers });
  const dc = pc.createDataChannel("ourchive-vfs", { ordered: true });
  dc.binaryType = "arraybuffer";

  return new Promise<{ blob: Blob; name: string }>(async (resolve, reject) => {
    let settled = false;
    const finishErr = (err: Error) => {
      if (settled) return;
      settled = true;
      pc.close();
      reject(err);
    };

    const connectTimer = window.setTimeout(() => {
      finishErr(new Error("WebRTC P2P connection timed out"));
    }, 10000);

    let fileName = relPath.split("/").pop() || "download";
    let mimeType = "application/octet-stream";
    let totalSize = 0;
    let received = 0;
    const chunks: ArrayBuffer[] = [];

    dc.onopen = () => {
      window.clearTimeout(connectTimer);
      dc.send(JSON.stringify({ action: "get", mount_id: mountId, path: relPath }));
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

    try {
      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      await waitForIceGathering(pc, 4000);
      if (!pc.localDescription?.sdp) {
        finishErr(new Error("Missing local SDP"));
        return;
      }
      const { sdp: answerSDP } = await api.sendP2POffer(nodeId, pc.localDescription.sdp);
      await pc.setRemoteDescription({ type: "answer", sdp: answerSDP });
    } catch (err) {
      window.clearTimeout(connectTimer);
      finishErr(err instanceof Error ? err : new Error("P2P signaling failed"));
    }
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
 * Downloads a file preferring direct WebRTC P2P (zero Hub egress),
 * falling back to the Hub yamux relay only when allowed by the Host Admin.
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
    try {
      const { blob, name } = await fetchViaWebRTC(nodeId, mountId, relPath, netCfg?.stun_servers ?? [], onProgress);
      triggerBlobDownload(blob, name);
      return "p2p";
    } catch (p2pErr) {
      if (netCfg && !netCfg.allow_relay_fallback) {
        throw new Error(
          `Direct P2P transfer failed (${p2pErr instanceof Error ? p2pErr.message : "NAT blocked"}) and Hub relay fallback is disabled by the Host Admin.`,
        );
      }
    }
  } else if (netCfg && !netCfg.allow_relay_fallback) {
    throw new Error("Both WebRTC P2P and Hub relay fallback are disabled by the Host Admin.");
  }

  // Fallback to Hub relay streaming download.
  onProgress?.({ received: 0, total: 0, transport: "relay" });
  const url = api.fileUrl(nodeId, mountId, relPath, true);
  const a = document.createElement("a");
  a.href = url;
  document.body.appendChild(a);
  a.click();
  a.remove();
  return "relay";
}

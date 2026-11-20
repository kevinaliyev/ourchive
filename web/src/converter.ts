export interface BrowserConvertResult {
  blob: Blob;
  filename: string;
  tierLabel: string;
  encoder: string;
  durationMs: number;
}

const BROWSER_IMAGE_OUTPUTS = new Set(["webp", "png", "jpg", "jpeg", "avif"]);
const BROWSER_DOC_OUTPUTS = new Set(["html", "csv", "json", "txt"]);

export function canConvertInBrowser(file: File, targetFormat: string): boolean {
  const fmt = targetFormat.toLowerCase();
  const mime = file.type.toLowerCase();
  const ext = file.name.slice(file.name.lastIndexOf(".") + 1).toLowerCase();

  if ((mime.startsWith("image/") || ["png", "jpg", "jpeg", "webp", "gif", "bmp", "svg", "avif"].includes(ext)) && BROWSER_IMAGE_OUTPUTS.has(fmt)) {
    return true;
  }
  if ((mime.startsWith("audio/") || ["mp3", "wav", "ogg", "m4a", "flac", "webm"].includes(ext)) && fmt === "wav") {
    return true;
  }
  if (["md", "txt", "json", "csv"].includes(ext) && BROWSER_DOC_OUTPUTS.has(fmt)) {
    return true;
  }
  return false;
}

export async function convertInBrowser(file: File, targetFormat: string, preset: string): Promise<BrowserConvertResult> {
  const start = performance.now();
  const fmt = targetFormat.toLowerCase();
  const baseName = file.name.replace(/\.[^.]+$/, "");
  const outName = `${baseName}.${fmt}`;

  if (BROWSER_IMAGE_OUTPUTS.has(fmt)) {
    const bitmap = await createImageBitmap(file);
    let width = bitmap.width;
    let height = bitmap.height;
    if (preset === "discord_10mb" && width > 1920) {
      height = Math.round((height * 1920) / width);
      width = 1920;
    }

    const canvas = document.createElement("canvas");
    canvas.width = width;
    canvas.height = height;
    const ctx = canvas.getContext("2d");
    if (!ctx) throw new Error("Canvas 2D context unavailable");
    ctx.drawImage(bitmap, 0, 0, width, height);
    bitmap.close();

    const outMime = fmt === "jpg" || fmt === "jpeg" ? "image/jpeg" : `image/${fmt}`;
    const quality = preset === "high_quality" ? 0.95 : preset === "discord_10mb" ? 0.78 : 0.86;
    const blob = await new Promise<Blob>((resolve, reject) => {
      canvas.toBlob((b) => (b ? resolve(b) : reject(new Error(`Browser does not support encoding ${fmt}`))), outMime, quality);
    });

    return {
      blob,
      filename: outName,
      tierLabel: "Tier 2 · Browser GPU Canvas",
      encoder: `canvas/${fmt}`,
      durationMs: Math.round(performance.now() - start),
    };
  }

  if (fmt === "wav") {
    const arrayBuf = await file.arrayBuffer();
    const audioCtx = new AudioContext();
    try {
      const audioBuf = await audioCtx.decodeAudioData(arrayBuf);
      const wavBlob = encodeAudioBufferToWav(audioBuf);
      return {
        blob: wavBlob,
        filename: outName,
        tierLabel: "Tier 2 · Browser WebAudio",
        encoder: "pcm_s16le",
        durationMs: Math.round(performance.now() - start),
      };
    } finally {
      await audioCtx.close();
    }
  }

  // Document / structured text conversions
  const text = await file.text();
  let outText = text;
  let outMime = "text/plain;charset=utf-8";

  if (fmt === "html") {
    outMime = "text/html;charset=utf-8";
    const escaped = text.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
    outText = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>${baseName}</title><style>body{font-family:system-ui,sans-serif;max-width:760px;margin:40px auto;padding:0 20px;line-height:1.6}pre{white-space:pre-wrap}</style></head><body><pre>${escaped}</pre></body></html>`;
  } else if (fmt === "csv") {
    outMime = "text/csv;charset=utf-8";
    const parsed = JSON.parse(text);
    const rows = Array.isArray(parsed) ? parsed : [parsed];
    if (rows.length === 0) {
      outText = "";
    } else {
      const headers = Array.from(new Set(rows.flatMap((r) => (r && typeof r === "object" ? Object.keys(r) : []))));
      const esc = (v: unknown) => `"${String(v ?? "").replace(/"/g, '""')}"`;
      outText = [headers.join(","), ...rows.map((r) => headers.map((h) => esc((r as Record<string, unknown>)[h])).join(","))].join("\n");
    }
  } else if (fmt === "json") {
    outMime = "application/json;charset=utf-8";
    const lines = text.trim().split(/\r?\n/);
    if (lines.length >= 2) {
      const headers = lines[0].split(",").map((h) => h.trim().replace(/^"|"$/g, ""));
      const objs = lines.slice(1).map((line) => {
        const vals = line.split(",").map((v) => v.trim().replace(/^"|"$/g, ""));
        const row: Record<string, string> = {};
        headers.forEach((h, i) => (row[h] = vals[i] ?? ""));
        return row;
      });
      outText = JSON.stringify(objs, null, 2);
    }
  }

  return {
    blob: new Blob([outText], { type: outMime }),
    filename: outName,
    tierLabel: "Tier 3 · Browser Local",
    encoder: `local/${fmt}`,
    durationMs: Math.round(performance.now() - start),
  };
}

function encodeAudioBufferToWav(buffer: AudioBuffer): Blob {
  const numChannels = buffer.numberOfChannels;
  const sampleRate = buffer.sampleRate;
  const length = buffer.length * numChannels * 2;
  const out = new ArrayBuffer(44 + length);
  const view = new DataView(out);

  const writeStr = (offset: number, str: string) => {
    for (let i = 0; i < str.length; i++) view.setUint8(offset + i, str.charCodeAt(i));
  };

  writeStr(0, "RIFF");
  view.setUint32(4, 36 + length, true);
  writeStr(8, "WAVE");
  writeStr(12, "fmt ");
  view.setUint32(16, 16, true);
  view.setUint16(20, 1, true); // PCM
  view.setUint16(22, numChannels, true);
  view.setUint32(24, sampleRate, true);
  view.setUint32(28, sampleRate * numChannels * 2, true);
  view.setUint16(32, numChannels * 2, true);
  view.setUint16(34, 16, true);
  writeStr(36, "data");
  view.setUint32(40, length, true);

  let offset = 44;
  for (let i = 0; i < buffer.length; i++) {
    for (let ch = 0; ch < numChannels; ch++) {
      const sample = Math.max(-1, Math.min(1, buffer.getChannelData(ch)[i]));
      view.setInt16(offset, sample < 0 ? sample * 0x8000 : sample * 0x7fff, true);
      offset += 2;
    }
  }
  return new Blob([out], { type: "audio/wav" });
}

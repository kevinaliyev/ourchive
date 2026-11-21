/**
 * converter.ts — Full vert.sh-parity format catalog + Tier 2/3 Browser Local Converter.
 * Supports Images (Canvas + pure-TS BMP/ICO), Audio (WebAudio WAV), and Documents
 * (pure-TS PDF 1.4, DOCX OOXML, RTF, HTML, Markdown, TXT, CSV, JSON, XML, YAML)
 * directly in the browser with zero server/Agent dependency, while routing heavy
 * video/audio/office jobs to Tier 1 Agent GPU/CPU ffmpeg & document engines.
 */

export type FormatCategory = "video" | "audio" | "image" | "document";

export interface FormatSpec {
  ext: string;
  label: string;
  category: FormatCategory;
  browserCapable?: boolean;
}

export const FORMAT_CATALOG: FormatSpec[] = [
  // Video (vert.sh parity)
  { ext: "mp4", label: "MP4 (H.264)", category: "video" },
  { ext: "webm", label: "WebM (VP9)", category: "video" },
  { ext: "mkv", label: "MKV (Matroska)", category: "video" },
  { ext: "mov", label: "MOV (QuickTime)", category: "video" },
  { ext: "avi", label: "AVI (MPEG-4)", category: "video" },
  { ext: "flv", label: "FLV (Flash)", category: "video" },
  { ext: "wmv", label: "WMV (Windows)", category: "video" },
  { ext: "m4v", label: "M4V", category: "video" },
  { ext: "ts", label: "TS (MPEG-TS)", category: "video" },
  { ext: "3gp", label: "3GP (Mobile)", category: "video" },
  { ext: "ogv", label: "OGV (Theora)", category: "video" },
  { ext: "gif", label: "GIF (Animated)", category: "video" },

  // Audio (vert.sh parity)
  { ext: "mp3", label: "MP3", category: "audio" },
  { ext: "wav", label: "WAV (PCM)", category: "audio", browserCapable: true },
  { ext: "flac", label: "FLAC (Lossless)", category: "audio" },
  { ext: "ogg", label: "OGG (Opus)", category: "audio" },
  { ext: "opus", label: "OPUS", category: "audio" },
  { ext: "m4a", label: "M4A (AAC)", category: "audio" },
  { ext: "aac", label: "AAC", category: "audio" },
  { ext: "wma", label: "WMA", category: "audio" },
  { ext: "aiff", label: "AIFF", category: "audio" },
  { ext: "ac3", label: "AC3 (Dolby)", category: "audio" },

  // Images (vert.sh parity)
  { ext: "webp", label: "WebP", category: "image", browserCapable: true },
  { ext: "png", label: "PNG", category: "image", browserCapable: true },
  { ext: "jpg", label: "JPG", category: "image", browserCapable: true },
  { ext: "avif", label: "AVIF", category: "image", browserCapable: true },
  { ext: "bmp", label: "BMP", category: "image", browserCapable: true },
  { ext: "ico", label: "ICO (Icon)", category: "image", browserCapable: true },
  { ext: "tiff", label: "TIFF", category: "image" },
  { ext: "tga", label: "TGA", category: "image" },
  { ext: "ppm", label: "PPM", category: "image" },

  // Documents & Data (vert.sh parity + Office/PDF)
  { ext: "pdf", label: "PDF", category: "document", browserCapable: true },
  { ext: "docx", label: "DOCX (Word)", category: "document", browserCapable: true },
  { ext: "odt", label: "ODT (OpenDoc)", category: "document" },
  { ext: "epub", label: "EPUB (eBook)", category: "document" },
  { ext: "rtf", label: "RTF", category: "document", browserCapable: true },
  { ext: "html", label: "HTML", category: "document", browserCapable: true },
  { ext: "md", label: "Markdown", category: "document", browserCapable: true },
  { ext: "txt", label: "Plain Text", category: "document", browserCapable: true },
  { ext: "csv", label: "CSV", category: "document", browserCapable: true },
  { ext: "json", label: "JSON", category: "document", browserCapable: true },
  { ext: "xml", label: "XML", category: "document", browserCapable: true },
  { ext: "yaml", label: "YAML", category: "document", browserCapable: true },
];

export interface BrowserConvertResult {
  blob: Blob;
  filename: string;
  tierLabel: string;
  encoder: string;
  durationMs: number;
}

const BROWSER_IMAGE_OUTPUTS = new Set(["webp", "png", "jpg", "jpeg", "avif", "bmp", "ico"]);
const BROWSER_DOC_OUTPUTS = new Set(["pdf", "docx", "rtf", "html", "md", "txt", "csv", "json", "xml", "yaml"]);
const BROWSER_DOC_INPUTS = new Set(["md", "markdown", "txt", "html", "htm", "rtf", "json", "csv", "xml", "yaml", "yml"]);

export function detectFileCategory(filename: string, mime = ""): FormatCategory {
  const lower = filename.toLowerCase();
  const ext = lower.includes(".") ? lower.slice(lower.lastIndexOf(".") + 1) : "";
  if (mime.startsWith("video/") || ["mp4", "mkv", "webm", "mov", "avi", "flv", "wmv", "m4v", "ts", "3gp", "ogv"].includes(ext)) {
    return "video";
  }
  if (mime.startsWith("audio/") || ["mp3", "wav", "flac", "ogg", "opus", "m4a", "aac", "wma", "aiff", "ac3"].includes(ext)) {
    return "audio";
  }
  if (mime.startsWith("image/") || ["png", "jpg", "jpeg", "webp", "gif", "bmp", "tiff", "tif", "ico", "svg", "avif", "heic", "heif", "tga", "ppm"].includes(ext)) {
    return "image";
  }
  return "document";
}

export function canConvertInBrowser(file: File, targetFormat: string): boolean {
  const fmt = targetFormat.toLowerCase();
  const mime = file.type.toLowerCase();
  const ext = file.name.includes(".") ? file.name.slice(file.name.lastIndexOf(".") + 1).toLowerCase() : "";

  if ((mime.startsWith("image/") || ["png", "jpg", "jpeg", "webp", "gif", "bmp", "svg", "avif"].includes(ext)) && BROWSER_IMAGE_OUTPUTS.has(fmt)) {
    return true;
  }
  if ((mime.startsWith("audio/") || ["mp3", "wav", "ogg", "m4a", "flac", "webm"].includes(ext)) && fmt === "wav") {
    return true;
  }
  if (BROWSER_DOC_INPUTS.has(ext) && BROWSER_DOC_OUTPUTS.has(fmt)) {
    return true;
  }
  return false;
}

export async function convertInBrowser(file: File, targetFormat: string, preset: string): Promise<BrowserConvertResult> {
  const start = performance.now();
  const fmt = targetFormat.toLowerCase();
  const ext = file.name.includes(".") ? file.name.slice(file.name.lastIndexOf(".") + 1).toLowerCase() : "";
  const baseName = file.name.replace(/\.[^.]+$/, "");
  const outName = `${baseName}.${fmt}`;

  if (BROWSER_IMAGE_OUTPUTS.has(fmt)) {
    const bitmap = await createImageBitmap(file);
    let width = bitmap.width;
    let height = bitmap.height;
    if (fmt === "ico" && (width > 256 || height > 256)) {
      const scale = Math.min(256 / width, 256 / height);
      width = Math.max(1, Math.round(width * scale));
      height = Math.max(1, Math.round(height * scale));
    } else if (preset === "discord_10mb" && width > 1920) {
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

    if (fmt === "bmp") {
      const imgData = ctx.getImageData(0, 0, width, height);
      const bmpBlob = encodeImageDataToBMP(imgData);
      return {
        blob: bmpBlob,
        filename: outName,
        tierLabel: "Tier 2 · Browser Canvas BMP",
        encoder: "canvas/bmp",
        durationMs: Math.round(performance.now() - start),
      };
    }

    if (fmt === "ico") {
      const pngBlob = await new Promise<Blob>((resolve, reject) => {
        canvas.toBlob((b) => (b ? resolve(b) : reject(new Error("PNG encode failed"))), "image/png");
      });
      const icoBlob = await wrapPngAsIco(pngBlob, width, height);
      return {
        blob: icoBlob,
        filename: outName,
        tierLabel: "Tier 2 · Browser Canvas ICO",
        encoder: "canvas/ico",
        durationMs: Math.round(performance.now() - start),
      };
    }

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

  // Document & Structured Data conversions
  const rawText = await file.text();
  const plainText = extractPlainTextBrowser(rawText, ext);

  if (fmt === "pdf") {
    const pdfBlob = new Blob([buildSimplePDF(baseName, plainText)], { type: "application/pdf" });
    return {
      blob: pdfBlob,
      filename: outName,
      tierLabel: "Tier 2 · Browser PDF Engine",
      encoder: "browser/pdf-1.4",
      durationMs: Math.round(performance.now() - start),
    };
  }

  if (fmt === "docx") {
    const docxBlob = new Blob([buildSimpleDOCX(plainText)], {
      type: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
    });
    return {
      blob: docxBlob,
      filename: outName,
      tierLabel: "Tier 2 · Browser DOCX Engine",
      encoder: "browser/ooxml",
      durationMs: Math.round(performance.now() - start),
    };
  }

  if (fmt === "rtf") {
    const rtfLines = plainText
      .replace(/\r\n/g, "\n")
      .split("\n")
      .map((l) => l.replace(/\\/g, "\\\\").replace(/\{/g, "\\{").replace(/\}/g, "\\}") + "\\par")
      .join("\n");
    const rtf = `{\\rtf1\\ansi\\deff0 {\\fonttbl {\\f0 Helvetica;}}\\f0\\fs22 ${rtfLines}}\n`;
    return {
      blob: new Blob([rtf], { type: "application/rtf" }),
      filename: outName,
      tierLabel: "Tier 2 · Browser Document",
      encoder: "browser/rtf",
      durationMs: Math.round(performance.now() - start),
    };
  }

  let outText = plainText;
  let outMime = "text/plain;charset=utf-8";

  if (fmt === "html") {
    outMime = "text/html;charset=utf-8";
    const escaped = escapeHtml(plainText);
    outText = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>${escapeHtml(baseName)}</title><style>body{font-family:system-ui,sans-serif;max-width:760px;margin:40px auto;padding:0 20px;line-height:1.6}pre{white-space:pre-wrap}</style></head><body><pre>${escaped}</pre></body></html>`;
  } else if (fmt === "csv") {
    outMime = "text/csv;charset=utf-8";
    try {
      const parsed = JSON.parse(rawText);
      const rows = Array.isArray(parsed) ? parsed : [parsed];
      const headers = Array.from(new Set(rows.flatMap((r: Record<string, unknown>) => Object.keys(r || {}))));
      const lines = [
        headers.join(","),
        ...rows.map((r: Record<string, unknown>) =>
          headers.map((h) => JSON.stringify(r?.[h] ?? "")).join(","),
        ),
      ];
      outText = lines.join("\n");
    } catch {
      outText =
        "line,text\n" +
        plainText
          .replace(/\r\n/g, "\n")
          .split("\n")
          .map((l, i) => `${i + 1},${JSON.stringify(l)}`)
          .join("\n");
    }
  } else if (fmt === "json") {
    outMime = "application/json;charset=utf-8";
    if (ext === "csv") {
      const lines = rawText.trim().split(/\r?\n/);
      const headers = (lines.shift() || "").split(",").map((h) => h.trim().replace(/^"|"$/g, ""));
      const objs = lines.map((line) => {
        const vals = line.split(",").map((v) => v.trim().replace(/^"|"$/g, ""));
        const o: Record<string, string> = {};
        headers.forEach((h, i) => {
          o[h] = vals[i] ?? "";
        });
        return o;
      });
      outText = JSON.stringify(objs, null, 2);
    } else {
      outText = JSON.stringify({ title: baseName, content: plainText }, null, 2);
    }
  } else if (fmt === "xml") {
    outMime = "application/xml;charset=utf-8";
    outText = `<?xml version="1.0" encoding="UTF-8"?>\n<document>\n  <title>${escapeHtml(baseName)}</title>\n  <body>${escapeHtml(plainText)}</body>\n</document>\n`;
  } else if (fmt === "yaml") {
    outMime = "application/yaml;charset=utf-8";
    const indented = plainText
      .replace(/\r\n/g, "\n")
      .split("\n")
      .map((l) => `  ${l}`)
      .join("\n");
    outText = `title: ${JSON.stringify(baseName)}\ncontent: |\n${indented}\n`;
  }

  return {
    blob: new Blob([outText], { type: outMime }),
    filename: outName,
    tierLabel: "Tier 2 · Browser Document",
    encoder: `browser/${fmt}`,
    durationMs: Math.round(performance.now() - start),
  };
}

function escapeHtml(s: string): string {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}

function extractPlainTextBrowser(raw: string, ext: string): string {
  if (ext === "html" || ext === "htm") {
    return raw
      .replace(/<(script|style)[^>]*>[\s\S]*?<\/\1>/gi, "")
      .replace(/<\/?(p|div|br|h[1-6]|li|tr)[^>]*>/gi, "\n")
      .replace(/<[^>]+>/g, "")
      .trim();
  }
  if (ext === "rtf") {
    return raw
      .replace(/\\par\b/g, "\n")
      .replace(/\\[a-zA-Z]+-?\d* ?|\\'[0-9a-fA-F]{2}|[{}]/g, "")
      .trim();
  }
  return raw;
}

function encodeImageDataToBMP(img: ImageData): Blob {
  const { width, height, data } = img;
  const rowSize = Math.floor((24 * width + 31) / 32) * 4;
  const pixelBytes = rowSize * height;
  const fileSize = 54 + pixelBytes;
  const buf = new ArrayBuffer(fileSize);
  const view = new DataView(buf);
  const bytes = new Uint8Array(buf);

  view.setUint8(0, 0x42); // B
  view.setUint8(1, 0x4d); // M
  view.setUint32(2, fileSize, true);
  view.setUint32(10, 54, true);
  view.setUint32(14, 40, true);
  view.setInt32(18, width, true);
  view.setInt32(22, height, true);
  view.setUint16(26, 1, true);
  view.setUint16(28, 24, true);
  view.setUint32(34, pixelBytes, true);

  for (let y = 0; y < height; y++) {
    const srcRow = (height - 1 - y) * width * 4;
    const dstRow = 54 + y * rowSize;
    for (let x = 0; x < width; x++) {
      bytes[dstRow + x * 3] = data[srcRow + x * 4 + 2];
      bytes[dstRow + x * 3 + 1] = data[srcRow + x * 4 + 1];
      bytes[dstRow + x * 3 + 2] = data[srcRow + x * 4];
    }
  }
  return new Blob([buf], { type: "image/bmp" });
}

async function wrapPngAsIco(pngBlob: Blob, width: number, height: number): Promise<Blob> {
  const pngBytes = new Uint8Array(await pngBlob.arrayBuffer());
  const header = new ArrayBuffer(22);
  const dv = new DataView(header);
  dv.setUint16(0, 0, true);
  dv.setUint16(2, 1, true); // ICO
  dv.setUint16(4, 1, true); // 1 image
  dv.setUint8(6, width >= 256 ? 0 : width);
  dv.setUint8(7, height >= 256 ? 0 : height);
  dv.setUint16(10, 1, true);
  dv.setUint16(12, 32, true);
  dv.setUint32(14, pngBytes.byteLength, true);
  dv.setUint32(18, 22, true);
  return new Blob([header, pngBytes], { type: "image/x-icon" });
}

function buildSimplePDF(title: string, body: string): string {
  const esc = (s: string) =>
    s
      .replace(/[^\x20-\x7e]/g, " ")
      .replace(/\\/g, "\\\\")
      .replace(/\(/g, "\\(")
      .replace(/\)/g, "\\)");

  const lines = [title, "", ...body.replace(/\r\n/g, "\n").split("\n")].slice(0, 48);
  const stream =
    "BT\n/F1 11 Tf\n50 742 Td\n14 TL\n" +
    lines.map((l, i) => (i === 0 ? `(${esc(l)}) Tj` : `T* (${esc(l)}) Tj`)).join("\n") +
    "\nET\n";

  const chunks: string[] = ["%PDF-1.4\n"];
  const offsets: number[] = [0];
  let len = chunks[0].length;
  const addObj = (n: number, content: string) => {
    offsets.push(len);
    const s = `${n} 0 obj\n${content}\nendobj\n`;
    chunks.push(s);
    len += s.length;
  };
  addObj(1, "<< /Type /Catalog /Pages 2 0 R >>");
  addObj(2, "<< /Type /Pages /Kids [4 0 R] /Count 1 >>");
  addObj(3, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>");
  addObj(4, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents 5 0 R >>");
  addObj(5, `<< /Length ${stream.length} >>\nstream\n${stream}endstream`);

  const xrefPos = len;
  chunks.push(`xref\n0 ${offsets.length}\n0000000000 65535 f \n`);
  for (let i = 1; i < offsets.length; i++) {
    chunks.push(`${String(offsets[i]).padStart(10, "0")} 00000 n \n`);
  }
  chunks.push(`trailer\n<< /Size ${offsets.length} /Root 1 0 R >>\nstartxref\n${xrefPos}\n%%EOF\n`);
  return chunks.join("");
}

function buildSimpleDOCX(body: string): Uint8Array<ArrayBuffer> {
  const enc = new TextEncoder();
  const paras = body
    .replace(/\r\n/g, "\n")
    .split("\n")
    .map((l) => `<w:p><w:r><w:t xml:space="preserve">${escapeHtml(l)}</w:t></w:r></w:p>`)
    .join("");
  const files: { name: string; data: Uint8Array }[] = [
    {
      name: "[Content_Types].xml",
      data: enc.encode(
        `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`,
      ),
    },
    {
      name: "_rels/.rels",
      data: enc.encode(
        `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`,
      ),
    },
    {
      name: "word/document.xml",
      data: enc.encode(
        `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>${paras}</w:body></w:document>`,
      ),
    },
  ];
  return buildStoredZip(files);
}

function crc32(bytes: Uint8Array): number {
  let c = 0xffffffff;
  for (let i = 0; i < bytes.length; i++) {
    c ^= bytes[i];
    for (let k = 0; k < 8; k++) {
      c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    }
  }
  return (c ^ 0xffffffff) >>> 0;
}

function buildStoredZip(entries: { name: string; data: Uint8Array }[]): Uint8Array<ArrayBuffer> {
  const enc = new TextEncoder();
  const parts: Uint8Array[] = [];
  const central: Uint8Array[] = [];
  let offset = 0;

  for (const e of entries) {
    const nameBytes = enc.encode(e.name);
    const crc = crc32(e.data);
    const lfh = new ArrayBuffer(30 + nameBytes.length);
    const dv = new DataView(lfh);
    dv.setUint32(0, 0x04034b50, true);
    dv.setUint16(4, 20, true);
    dv.setUint16(8, 0, true); // Store
    dv.setUint32(14, crc, true);
    dv.setUint32(18, e.data.length, true);
    dv.setUint32(22, e.data.length, true);
    dv.setUint16(26, nameBytes.length, true);
    new Uint8Array(lfh, 30).set(nameBytes);
    parts.push(new Uint8Array(lfh), e.data);

    const cdh = new ArrayBuffer(46 + nameBytes.length);
    const cdv = new DataView(cdh);
    cdv.setUint32(0, 0x02014b50, true);
    cdv.setUint16(4, 20, true);
    cdv.setUint16(6, 20, true);
    cdv.setUint32(16, crc, true);
    cdv.setUint32(20, e.data.length, true);
    cdv.setUint32(24, e.data.length, true);
    cdv.setUint16(28, nameBytes.length, true);
    cdv.setUint32(42, offset, true);
    new Uint8Array(cdh, 46).set(nameBytes);
    central.push(new Uint8Array(cdh));

    offset += 30 + nameBytes.length + e.data.length;
  }

  const centralSize = central.reduce((sum, b) => sum + b.length, 0);
  const eocd = new ArrayBuffer(22);
  const edv = new DataView(eocd);
  edv.setUint32(0, 0x06054b50, true);
  edv.setUint16(8, entries.length, true);
  edv.setUint16(10, entries.length, true);
  edv.setUint32(12, centralSize, true);
  edv.setUint32(16, offset, true);

  const totalLen = offset + centralSize + 22;
  const out = new Uint8Array(new ArrayBuffer(totalLen));
  let pos = 0;
  for (const p of [...parts, ...central, new Uint8Array(eocd)]) {
    out.set(p, pos);
    pos += p.length;
  }
  return out;
}

function encodeAudioBufferToWav(buffer: AudioBuffer): Blob {
  const numChannels = buffer.numberOfChannels;
  const sampleRate = buffer.sampleRate;
  const numFrames = buffer.length;
  const dataSize = numFrames * numChannels * 2;
  const arrayBuffer = new ArrayBuffer(44 + dataSize);
  const view = new DataView(arrayBuffer);

  const writeStr = (offset: number, s: string) => {
    for (let i = 0; i < s.length; i++) view.setUint8(offset + i, s.charCodeAt(i));
  };

  writeStr(0, "RIFF");
  view.setUint32(4, 36 + dataSize, true);
  writeStr(8, "WAVE");
  writeStr(12, "fmt ");
  view.setUint32(16, 16, true);
  view.setUint16(20, 1, true);
  view.setUint16(22, numChannels, true);
  view.setUint32(24, sampleRate, true);
  view.setUint32(28, sampleRate * numChannels * 2, true);
  view.setUint16(32, numChannels * 2, true);
  view.setUint16(34, 16, true);
  writeStr(36, "data");
  view.setUint32(40, dataSize, true);

  const channels: Float32Array[] = [];
  for (let ch = 0; ch < numChannels; ch++) {
    channels.push(buffer.getChannelData(ch));
  }

  let offset = 44;
  for (let i = 0; i < numFrames; i++) {
    for (let ch = 0; ch < numChannels; ch++) {
      const sample = Math.max(-1, Math.min(1, channels[ch][i]));
      view.setInt16(offset, sample < 0 ? sample * 0x8000 : sample * 0x7fff, true);
      offset += 2;
    }
  }

  return new Blob([arrayBuffer], { type: "audio/wav" });
}

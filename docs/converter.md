# 3-Tier Media & Document Converter

Ourchive includes a built-in converter targeting full format parity with `vert.sh` plus office documents and structured data. Conversions are automatically routed to the fastest available tier.

---

## Conversion Tiers

| Tier | Engine | When Used |
| :--- | :--- | :--- |
| **Tier 1 — Agent Native GPU** | Hardware-accelerated `ffmpeg` + LibreOffice (`soffice`) / `pandoc` / Pure-Go fallback | Default when an online contributor Agent with compute enabled is available |
| **Tier 2 — Browser Local** | Pure TypeScript + HTML Canvas / Web Audio | Zero-setup local conversion in the browser when offline or no compute Agent is selected |
| **Tier 3 — Hub Fallback** | Lightweight Hub-side routing & job history | Coordinates job state and history across nodes |

### Hardware GPU Detection
When an Agent starts (`ourchive-agent probe` / `run`), it probes local GPUs and verifies working `ffmpeg` hardware encoders before advertising them to the Hub:

- **AMD**: `h264_amf`, `hevc_amf`, `av1_amf`, VAAPI
- **NVIDIA**: `h264_nvenc`, `hevc_nvenc`, `av1_nvenc`
- **Intel**: `h264_qsv`, `hevc_qsv`, `av1_qsv`, VAAPI
- **Apple Silicon**: `h264_videotoolbox`, `hevc_videotoolbox`

Discrete GPUs are prioritized over integrated graphics, and software encoding (`libx264`, `libx265`, `libvpx-vp9`, `libaom-av1`) is used automatically if a target codec lacks hardware support.

---

## Supported Formats

### Video (Agent Tier 1)
`mp4` · `webm` · `mkv` · `mov` · `avi` · `flv` · `wmv` · `m4v` · `ts` · `3gp` · `ogv` · `gif`

### Audio (Agent Tier 1 + Browser WAV)
`mp3` · `wav` · `flac` · `ogg` · `opus` · `m4a` · `aac` · `wma` · `aiff` · `ac3`

### Images (Agent Tier 1 + Browser Tier 2)
`webp` · `png` · `jpg` · `avif` · `bmp` · `ico` · `tiff` · `tga` · `ppm`

*(Browser Tier 2 natively supports `webp`, `png`, `jpg`, `avif`, `bmp`, and `ico`.)*

### Documents (Agent Tier 1 + Browser Tier 2)
`pdf` · `docx` · `odt` · `epub` · `rtf` · `html` · `md` · `txt`

- **Agent Tier 1**: Uses LibreOffice (`soffice`) or `pandoc` when installed for rich layout fidelity, and seamlessly falls back to Ourchive's **zero-dependency pure-Go document engine** (`internal/compute/documents.go`) supporting valid multi-page PDF 1.4 generation/extraction, OOXML `.docx`, ODF `.odt`, EPUB 3 `.epub`, `.rtf`, `.html`, `.md`, and `.txt`.
- **Browser Tier 2**: Natively converts between `pdf`, `docx`, `rtf`, `html`, `md`, and `txt` directly in the browser with no Agent required.

### Structured Data (Agent Tier 1 + Browser Tier 2)
`csv` · `json` · `xml` · `yaml`

Bidirectional conversion across all four structured formats works identically on both Agent Tier 1 and Browser Tier 2.

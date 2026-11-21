package compute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ourchive/ourchive/pkg/protocol"
)

var candidateHWEncoders = []string{
	"h264_nvenc", "hevc_nvenc", "av1_nvenc",
	"h264_amf", "hevc_amf", "av1_amf",
	"h264_qsv", "hevc_qsv", "av1_qsv",
	"h264_videotoolbox", "hevc_videotoolbox",
}

// ProbeCompute detects CPU/GPU hardware names, ffmpeg, and runs a 1-frame runtime test
// on compiled-in hardware encoders so only working GPU drivers are advertised to the Hub.
func ProbeCompute() protocol.ComputeCapabilities {
	cpuModel, gpuModels := probeHardwareNames()
	primaryGPU := ""
	if len(gpuModels) > 0 {
		primaryGPU = gpuModels[0]
	}
	caps := protocol.ComputeCapabilities{
		CPUModel:      cpuModel,
		GPUModel:      primaryGPU,
		GPUModels:     gpuModels,
		CPUCores:      runtime.NumCPU(),
		MaxConcurrent: 2,
	}

	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return caps
	}
	caps.Enabled = true
	caps.FFmpegPath = ffmpegPath

	cmd := exec.Command(ffmpegPath, "-hide_banner", "-encoders")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return caps
	}
	output := out.String()

	var compiled []string
	for _, enc := range candidateHWEncoders {
		if strings.Contains(output, " "+enc+" ") {
			compiled = append(compiled, enc)
		}
	}

	// Verify each compiled-in encoder with a fast 1-frame null encode in parallel.
	var wg sync.WaitGroup
	var mu sync.Mutex
	working := make([]string, 0, len(compiled))
	for _, enc := range compiled {
		wg.Add(1)
		go func(encoder string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			testCmd := exec.CommandContext(ctx, ffmpegPath,
				"-hide_banner", "-loglevel", "error",
				"-f", "lavfi", "-i", "color=c=black:s=256x256:d=0.1",
				"-frames:v", "1",
				"-c:v", encoder,
				"-f", "null", "-",
			)
			if testCmd.Run() == nil {
				mu.Lock()
				working = append(working, encoder)
				mu.Unlock()
			}
		}(enc)
	}
	wg.Wait()
	caps.HWEncoders = working
	return caps
}

func probeHardwareNames() (cpuModel string, gpuModels []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	var rawGPUs []string

	switch runtime.GOOS {
	case "windows":
		script := `
$cpu = (Get-CimInstance Win32_Processor | Select-Object -First 1 -ExpandProperty Name)
$gpus = @(Get-CimInstance Win32_VideoController | Select-Object -ExpandProperty Name | Where-Object { $_ -and $_ -notmatch 'Basic Display|Virtual|Parsec|Remote|Meta|Oculus' })
[Console]::Out.Write((($cpu -replace '\s+',' ').Trim()) + "||" + (($gpus | ForEach-Object { ($_ -replace '\s+',' ').Trim() }) -join ";;"))
`
		out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", script).Output()
		if err == nil {
			parts := strings.SplitN(strings.TrimSpace(string(out)), "||", 2)
			if len(parts) >= 1 {
				cpuModel = strings.TrimSpace(parts[0])
			}
			if len(parts) == 2 && strings.TrimSpace(parts[1]) != "" {
				for _, g := range strings.Split(parts[1], ";;") {
					if g = strings.TrimSpace(g); g != "" {
						rawGPUs = append(rawGPUs, g)
					}
				}
			}
		}
	case "darwin":
		if out, err := exec.CommandContext(ctx, "sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			cpuModel = strings.TrimSpace(string(out))
		}
		if out, err := exec.CommandContext(ctx, "sh", "-c", "system_profiler SPDisplaysDataType | awk -F': ' '/Chipset Model/{print $2}'").Output(); err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if line = strings.TrimSpace(line); line != "" {
					rawGPUs = append(rawGPUs, line)
				}
			}
		}
	default:
		if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "model name") {
					if parts := strings.SplitN(line, ":", 2); len(parts) == 2 {
						cpuModel = strings.TrimSpace(parts[1])
						break
					}
				}
			}
		}
		if out, err := exec.CommandContext(ctx, "sh", "-c", "lspci | grep -iE 'vga|3d|display' | cut -d':' -f3-").Output(); err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if line = strings.TrimSpace(line); line != "" {
					rawGPUs = append(rawGPUs, line)
				}
			}
		}
	}

	// Prioritize discrete GPUs (RTX/GTX/GeForce, Radeon RX/Pro, Intel Arc, Quadro) ahead of integrated GPUs.
	gpuModels = sortGPUsByPriority(rawGPUs)
	return cpuModel, gpuModels
}

func gpuPriorityScore(name string) int {
	s := strings.ToLower(name)
	// Clearly discrete GPUs
	if strings.Contains(s, "rtx") || strings.Contains(s, "geforce") || strings.Contains(s, "gtx") ||
		strings.Contains(s, "quadro") || strings.Contains(s, "radeon rx") || strings.Contains(s, "radeon pro") ||
		strings.Contains(s, "intel arc") || strings.Contains(s, "arc a") || strings.Contains(s, "arc b") {
		return 100
	}
	// Clearly integrated GPUs
	if strings.Contains(s, "radeon(tm) graphics") || strings.Contains(s, "uhd graphics") ||
		strings.Contains(s, "iris") || strings.Contains(s, "hd graphics") || strings.Contains(s, "vega") {
		return 10
	}
	return 50
}

func sortGPUsByPriority(gpus []string) []string {
	seen := map[string]bool{}
	unique := make([]string, 0, len(gpus))
	for _, g := range gpus {
		if !seen[g] {
			seen[g] = true
			unique = append(unique, g)
		}
	}
	// Simple stable insertion sort descending by score.
	for i := 1; i < len(unique); i++ {
		for j := i; j > 0 && gpuPriorityScore(unique[j]) > gpuPriorityScore(unique[j-1]); j-- {
			unique[j], unique[j-1] = unique[j-1], unique[j]
		}
	}
	return unique
}

// SelectH264Encoder picks the best verified hardware H.264 encoder, falling back to libx264.
func SelectH264Encoder(hwEncoders []string) string {
	preferred := []string{"h264_nvenc", "h264_amf", "h264_qsv", "h264_videotoolbox"}
	for _, pref := range preferred {
		for _, avail := range hwEncoders {
			if avail == pref {
				return pref
			}
		}
	}
	return "libx264"
}

// ProbeDurationSeconds uses ffprobe (if installed) to read media duration for target-size presets.
func ProbeDurationSeconds(ctx context.Context, inputPath string) float64 {
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		return 0
	}
	cmd := exec.CommandContext(ctx, ffprobePath,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		inputPath,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	secs, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || secs <= 0 {
		return 0
	}
	return secs
}

// BuildFFmpegArgs builds deterministic ffmpeg arguments for a target format and preset.
func BuildFFmpegArgs(inputPath, outputPath, format, preset string, hwEncoders []string, durationSec float64) ([]string, string) {
	format = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(format), "."))
	preset = strings.ToLower(strings.TrimSpace(preset))

	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", inputPath}
	usedEncoder := "ffmpeg"

	switch format {
	case "mp4", "mov", "mkv", "m4v", "ts", "flv":
		vEnc := SelectH264Encoder(hwEncoders)
		usedEncoder = vEnc
		args = append(args, "-c:v", vEnc)
		if preset == "discord_10mb" {
			// Target <= 9.2 MB total (leaving headroom under Discord's 10 MB cap).
			audioKbps := 96
			videoKbps := 1200
			if durationSec > 0 {
				totalKbps := int((9.2 * 8 * 1024) / durationSec)
				videoKbps = totalKbps - audioKbps
				if videoKbps < 200 {
					videoKbps = 200
				}
				if videoKbps > 6000 {
					videoKbps = 6000
				}
			}
			args = append(args,
				"-vf", "scale='min(1280,iw)':-2",
				"-b:v", fmt.Sprintf("%dk", videoKbps),
				"-maxrate", fmt.Sprintf("%dk", videoKbps+200),
				"-bufsize", fmt.Sprintf("%dk", videoKbps*2),
				"-c:a", "aac", "-b:a", fmt.Sprintf("%dk", audioKbps),
			)
		} else if preset == "web_fast" {
			args = append(args, "-vf", "scale='min(1920,iw)':-2", "-c:a", "aac", "-b:a", "128k")
		} else {
			args = append(args, "-c:a", "aac", "-b:a", "192k")
		}
		if vEnc == "libx264" {
			args = append(args, "-pix_fmt", "yuv420p", "-preset", "fast")
		}
		if format == "mp4" || format == "mov" || format == "m4v" {
			args = append(args, "-movflags", "+faststart")
		}

	case "avi":
		usedEncoder = "mpeg4"
		args = append(args, "-c:v", "mpeg4", "-q:v", "4", "-c:a", "libmp3lame", "-q:a", "3")
	case "wmv":
		usedEncoder = "wmv2"
		args = append(args, "-c:v", "wmv2", "-b:v", "2500k", "-c:a", "wmav2", "-b:a", "128k")
	case "3gp":
		usedEncoder = "libx264"
		args = append(args, "-c:v", "libx264", "-pix_fmt", "yuv420p", "-vf", "scale='min(640,iw)':-2", "-c:a", "aac", "-b:a", "96k")
	case "ogv":
		usedEncoder = "libtheora"
		args = append(args, "-c:v", "libtheora", "-q:v", "6", "-c:a", "libvorbis", "-q:a", "4")

	case "webm":
		usedEncoder = "libvpx-vp9"
		args = append(args, "-c:v", "libvpx-vp9", "-crf", "32", "-b:v", "0", "-c:a", "libopus", "-b:a", "128k")
		if preset == "discord_10mb" {
			args = append(args, "-vf", "scale='min(1280,iw)':-2")
		}

	case "gif":
		usedEncoder = "gif-palette"
		args = append(args, "-vf", "fps=15,scale='min(640,iw)':-1:flags=lanczos,split[s0][s1];[s0]palettegen[p];[s1][p]paletteuse")

	case "mp3":
		usedEncoder = "libmp3lame"
		args = append(args, "-vn", "-c:a", "libmp3lame", "-q:a", "2")
	case "wav":
		usedEncoder = "pcm_s16le"
		args = append(args, "-vn", "-c:a", "pcm_s16le")
	case "flac":
		usedEncoder = "flac"
		args = append(args, "-vn", "-c:a", "flac")
	case "ogg":
		usedEncoder = "libopus"
		args = append(args, "-vn", "-c:a", "libopus", "-b:a", "128k")
	case "opus":
		usedEncoder = "libopus"
		args = append(args, "-vn", "-c:a", "libopus", "-b:a", "128k")
	case "m4a":
		usedEncoder = "aac"
		args = append(args, "-vn", "-c:a", "aac", "-b:a", "192k")
	case "aac":
		usedEncoder = "aac"
		args = append(args, "-vn", "-c:a", "aac", "-b:a", "192k")
	case "wma":
		usedEncoder = "wmav2"
		args = append(args, "-vn", "-c:a", "wmav2", "-b:a", "192k")
	case "aiff":
		usedEncoder = "pcm_s16be"
		args = append(args, "-vn", "-c:a", "pcm_s16be")
	case "ac3":
		usedEncoder = "ac3"
		args = append(args, "-vn", "-c:a", "ac3", "-b:a", "192k")

	case "webp":
		usedEncoder = "libwebp"
		args = append(args, "-frames:v", "1", "-c:v", "libwebp", "-q:v", "82")
	case "jpg", "jpeg":
		usedEncoder = "mjpeg"
		args = append(args, "-frames:v", "1", "-q:v", "3")
	case "png":
		usedEncoder = "png"
		args = append(args, "-frames:v", "1")
	case "avif":
		usedEncoder = "libaom-av1"
		args = append(args, "-frames:v", "1", "-c:v", "libaom-av1", "-still-picture", "1", "-crf", "30")
	case "bmp":
		usedEncoder = "bmp"
		args = append(args, "-frames:v", "1", "-c:v", "bmp")
	case "tiff", "tif":
		usedEncoder = "tiff"
		args = append(args, "-frames:v", "1", "-c:v", "tiff")
	case "ico":
		usedEncoder = "ico"
		args = append(args, "-frames:v", "1", "-vf", "scale='min(256,iw)':'min(256,ih)':force_original_aspect_ratio=decrease")
	case "tga":
		usedEncoder = "targa"
		args = append(args, "-frames:v", "1", "-c:v", "targa")
	case "ppm":
		usedEncoder = "ppm"
		args = append(args, "-frames:v", "1", "-c:v", "ppm")
	}

	args = append(args, outputPath)
	return args, usedEncoder
}

// ConvertRunner executes ffmpeg conversion jobs on an Agent with concurrency limiting.
type ConvertRunner struct {
	Caps protocol.ComputeCapabilities
	sem  chan struct{}
}

func NewConvertRunner(caps protocol.ComputeCapabilities) *ConvertRunner {
	maxC := caps.MaxConcurrent
	if maxC < 1 {
		maxC = 2
	}
	return &ConvertRunner{
		Caps: caps,
		sem:  make(chan struct{}, maxC),
	}
}

// ServeHTTP handles `POST /compute/convert?format=mp4&preset=discord_10mb&filename=clip.mkv`
// by reading the input stream, running hardware ffmpeg or the built-in document engine, and streaming the output back.
func (cr *ConvertRunner) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	format := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(r.URL.Query().Get("format")), "."))
	preset := strings.TrimSpace(r.URL.Query().Get("preset"))
	origName := strings.TrimSpace(r.URL.Query().Get("filename"))
	if format == "" {
		http.Error(w, `{"error":"target format is required"}`, http.StatusBadRequest)
		return
	}
	if origName == "" {
		origName = "input.bin"
	}
	isDoc := IsDocumentFormat(format)
	if !isDoc && cr.Caps.FFmpegPath == "" {
		http.Error(w, `{"error":"ffmpeg is not available on this machine"}`, http.StatusServiceUnavailable)
		return
	}

	select {
	case cr.sem <- struct{}{}:
		defer func() { <-cr.sem }()
	case <-r.Context().Done():
		http.Error(w, `{"error":"cancelled"}`, http.StatusRequestTimeout)
		return
	}

	tmpDir, err := os.MkdirTemp("", "ourchive-conv-*")
	if err != nil {
		http.Error(w, `{"error":"failed to allocate temp workspace"}`, http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmpDir)

	inExt := filepath.Ext(origName)
	inPath := filepath.Join(tmpDir, "input"+inExt)
	inFile, err := os.Create(inPath)
	if err != nil {
		http.Error(w, `{"error":"failed to create temp input"}`, http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(inFile, r.Body); err != nil {
		_ = inFile.Close()
		http.Error(w, `{"error":"failed to read input stream"}`, http.StatusBadRequest)
		return
	}
	_ = inFile.Close()

	baseName := strings.TrimSuffix(filepath.Base(origName), inExt)
	outName := baseName + "." + format
	outPath := filepath.Join(tmpDir, "output."+format)

	start := time.Now()
	convCtx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	var usedEnc string
	if isDoc {
		docEnc, docErr := ConvertDocument(convCtx, inPath, outPath, inExt, format, baseName)
		if docErr != nil {
			writeJSONErr(w, http.StatusUnprocessableEntity, fmt.Sprintf("document conversion failed: %s", docErr.Error()))
			return
		}
		usedEnc = docEnc
	} else {
		dur := ProbeDurationSeconds(r.Context(), inPath)
		var args []string
		args, usedEnc = BuildFFmpegArgs(inPath, outPath, format, preset, cr.Caps.HWEncoders, dur)

		var stderr bytes.Buffer
		cmd := exec.CommandContext(convCtx, cr.Caps.FFmpegPath, args...)
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			// If a hardware encoder failed mid-run, retry once with software libx264 for video formats.
			if strings.Contains(usedEnc, "nvenc") || strings.Contains(usedEnc, "amf") || strings.Contains(usedEnc, "qsv") {
				stderr.Reset()
				swArgs, swEnc := BuildFFmpegArgs(inPath, outPath, format, preset, nil, dur)
				swCmd := exec.CommandContext(convCtx, cr.Caps.FFmpegPath, swArgs...)
				swCmd.Stderr = &stderr
				if swErr := swCmd.Run(); swErr != nil {
					writeJSONErr(w, http.StatusUnprocessableEntity, fmt.Sprintf("ffmpeg failed: %s", trimErr(stderr.String(), swErr)))
					return
				}
				usedEnc = swEnc
			} else {
				writeJSONErr(w, http.StatusUnprocessableEntity, fmt.Sprintf("ffmpeg failed: %s", trimErr(stderr.String(), err)))
				return
			}
		}
	}

	outFile, err := os.Open(outPath)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "converted file missing")
		return
	}
	defer outFile.Close()
	info, _ := outFile.Stat()

	w.Header().Set("Content-Type", mimeForFormat(format))
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, outName))
	if info != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	}
	w.Header().Set("X-Ourchive-Encoder", usedEnc)
	w.Header().Set("X-Ourchive-Duration-Ms", strconv.FormatInt(time.Since(start).Milliseconds(), 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, outFile)
}

func writeJSONErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func trimErr(stderr string, err error) string {
	s := strings.TrimSpace(stderr)
	if s == "" && err != nil {
		return err.Error()
	}
	if len(s) > 240 {
		return s[len(s)-240:]
	}
	if s == "" {
		return errors.New("unknown conversion error").Error()
	}
	return s
}

func mimeForFormat(format string) string {
	switch format {
	case "mp4":
		return "video/mp4"
	case "webm":
		return "video/webm"
	case "mov":
		return "video/quicktime"
	case "mkv":
		return "video/x-matroska"
	case "gif":
		return "image/gif"
	case "mp3":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	case "flac":
		return "audio/flac"
	case "ogg":
		return "audio/ogg"
	case "m4a":
		return "audio/mp4"
	case "webp":
		return "image/webp"
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "avif":
		return "image/avif"
	case "bmp":
		return "image/bmp"
	case "tiff", "tif":
		return "image/tiff"
	case "ico":
		return "image/x-icon"
	case "avi":
		return "video/x-msvideo"
	case "flv":
		return "video/x-flv"
	case "wmv":
		return "video/x-ms-wmv"
	case "opus":
		return "audio/opus"
	case "aac":
		return "audio/aac"
	case "aiff":
		return "audio/aiff"
	case "pdf":
		return "application/pdf"
	case "docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case "odt":
		return "application/vnd.oasis.opendocument.text"
	case "epub":
		return "application/epub+zip"
	case "rtf":
		return "application/rtf"
	case "html", "htm":
		return "text/html; charset=utf-8"
	case "md", "markdown":
		return "text/markdown; charset=utf-8"
	case "txt":
		return "text/plain; charset=utf-8"
	case "csv":
		return "text/csv; charset=utf-8"
	case "json":
		return "application/json"
	case "xml":
		return "application/xml"
	case "yaml", "yml":
		return "application/yaml"
	default:
		return "application/octet-stream"
	}
}

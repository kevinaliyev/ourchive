package vfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// handleThumb lazily generates a small WebP thumbnail for images and videos via ffmpeg
// (or falls back to serving small images directly when ffmpeg is not installed).
func (s *AgentServer) handleThumb(w http.ResponseWriter, r *http.Request) {
	m, _, err := s.findMount(r.PathValue("mountId"))
	if err != nil {
		writeVFSError(w, err)
		return
	}

	f, info, cleanup, err := s.openMountFile(r.PathValue("mountId"), r.URL.Query().Get("path"))
	if err != nil {
		writeVFSError(w, err)
		return
	}
	defer cleanup()
	if info.IsDir() {
		f.Close()
		http.Error(w, `{"error":"path is not a file"}`, http.StatusBadRequest)
		return
	}

	rel, _ := CleanRelPath(r.URL.Query().Get("path"))
	mimeType := detectMimeByExt(info.Name())
	isImage := strings.HasPrefix(mimeType, "image/")
	isVideo := strings.HasPrefix(mimeType, "video/")
	if !isImage && !isVideo {
		f.Close()
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Compute stable ETag from mount + rel + size + modTime.
	etagSum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%d", r.PathValue("mountId"), rel, info.Size(), info.ModTime().UnixNano())))
	etag := `"` + hex.EncodeToString(etagSum[:8]) + `"`
	if r.Header.Get("If-None-Match") == etag {
		f.Close()
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Resolve real filesystem path for ffmpeg after os.Root verified the entry.
	mountLocalPath, _ := ExpandLocalPath(m.LocalPath)
	fullPath := mountLocalPath
	if m.Kind != "file" {
		fullPath = filepath.Join(mountLocalPath, filepath.FromSlash(rel))
	}

	if ffmpegPath, err := exec.LookPath("ffmpeg"); err == nil && mimeType != "image/svg+xml" {
		f.Close()
		thumbCtx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		defer cancel()

		thumbBytes, err := generateWebPThumb(thumbCtx, ffmpegPath, fullPath, isVideo)
		if err == nil && len(thumbBytes) > 0 {
			if s.Meter != nil {
				s.Meter.AddUpload(int64(len(thumbBytes)))
			}
			w.Header().Set("Content-Type", "image/webp")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(thumbBytes)
			return
		}
		// If ffmpeg failed on an image, reopen and fall back below.
		if isImage {
			f, _, cleanup2, err := s.openMountFile(r.PathValue("mountId"), r.URL.Query().Get("path"))
			if err != nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			defer cleanup2()
			defer f.Close()
			if info.Size() <= 4*1024*1024 {
				w.Header().Set("Content-Type", mimeType)
				w.Header().Set("Cache-Control", "public, max-age=86400")
				w.Header().Set("ETag", etag)
				n, _ := io.Copy(w, f)
				if s.Meter != nil {
					s.Meter.AddUpload(n)
				}
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	defer f.Close()

	// Fallback when ffmpeg is absent: serve small images (<= 4 MB) directly.
	if isImage && info.Size() <= 4*1024*1024 {
		w.Header().Set("Content-Type", mimeType)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Header().Set("ETag", etag)
		n, _ := io.Copy(w, f)
		if s.Meter != nil {
			s.Meter.AddUpload(n)
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func generateWebPThumb(ctx context.Context, ffmpegPath, inputPath string, isVideo bool) ([]byte, error) {
	run := func(seek string) ([]byte, error) {
		args := []string{"-hide_banner", "-loglevel", "error"}
		if seek != "" {
			args = append(args, "-ss", seek)
		}
		args = append(args,
			"-i", inputPath,
			"-frames:v", "1",
			"-vf", "scale='min(480,iw)':-2",
			"-c:v", "libwebp",
			"-q:v", "75",
			"-f", "webp",
			"pipe:1",
		)
		cmd := command(ctx, ffmpegPath, args...)
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			return nil, err
		}
		return out.Bytes(), nil
	}

	if isVideo {
		if b, err := run("00:00:01"); err == nil && len(b) > 0 {
			return b, nil
		}
		return run("00:00:00")
	}
	return run("")
}

package vfs

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// handleZip streams a directory as an on-the-fly ZIP archive directly from os.Root
// without creating any temporary file on disk.
func (s *AgentServer) handleZip(w http.ResponseWriter, r *http.Request) {
	root, cfg, err := s.openMountRoot(r.PathValue("mountId"))
	if err != nil {
		writeVFSError(w, err)
		return
	}
	defer root.Close()

	rel, err := CleanRelPath(r.URL.Query().Get("path"))
	if err != nil {
		writeVFSError(w, err)
		return
	}

	target := "."
	zipName := "archive.zip"
	if rel != "" {
		target = filepath.FromSlash(rel)
		zipName = path.Base(rel) + ".zip"
	} else {
		for _, m := range cfg.Mounts {
			if m.ID == r.PathValue("mountId") && m.Name != "" {
				zipName = m.Name + ".zip"
				break
			}
		}
	}

	info, err := root.Stat(target)
	if err != nil {
		writeVFSError(w, err)
		return
	}
	if !info.IsDir() {
		http.Error(w, `{"error":"path is not a directory"}`, http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, zipName))
	w.WriteHeader(http.StatusOK)

	var dst io.Writer = w
	if s.Meter != nil || cfg.UploadCapMbps > 0 {
		dst = &meteredWriter{
			ResponseWriter: w,
			meter:          s.Meter,
			capBytesPerSec: int64(cfg.UploadCapMbps) * 125_000,
			startTime:      time.Now(),
		}
	}

	zw := zip.NewWriter(dst)
	defer zw.Close()

	rootFS := root.FS()
	walkRoot := "."
	if rel != "" {
		walkRoot = rel
	}

	_ = fs.WalkDir(rootFS, walkRoot, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		// Compute relative path inside the ZIP archive.
		entryName := p
		if walkRoot != "." {
			entryName = strings.TrimPrefix(p, walkRoot+"/")
		}
		if entryName == "" || entryName == "." {
			return nil
		}

		fi, err := d.Info()
		if err != nil {
			return nil
		}
		hdr, err := zip.FileInfoHeader(fi)
		if err != nil {
			return nil
		}
		hdr.Name = entryName
		// Use Store (no recompression) for already-compressed media, Deflate for text/misc.
		if isAlreadyCompressed(entryName) {
			hdr.Method = zip.Store
		} else {
			hdr.Method = zip.Deflate
		}

		entryWriter, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		src, err := rootFS.Open(p)
		if err != nil {
			return nil
		}
		_, copyErr := io.Copy(entryWriter, src)
		_ = src.Close()
		return copyErr
	})
}

func isAlreadyCompressed(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	switch ext {
	case ".mp4", ".m4v", ".webm", ".mkv", ".mov", ".mp3", ".ogg", ".opus", ".flac",
		".jpg", ".jpeg", ".png", ".webp", ".gif", ".zip", ".gz", ".7z", ".rar", ".zst":
		return true
	}
	return false
}

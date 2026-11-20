package vfs

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const sampleChunkSize = 64 * 1024 // 64 KB head + mid + tail = <2ms even on 50 GB files

// ComputeFastHash computes a fast structural fingerprint from file size + head/mid/tail 64KB slices.
func ComputeFastHash(r io.ReadSeeker, size int64) (string, error) {
	h := sha256.New()
	var sizeBuf [8]byte
	binary.LittleEndian.PutUint64(sizeBuf[:], uint64(size))
	h.Write(sizeBuf[:])

	buf := make([]byte, sampleChunkSize)
	readSlice := func(offset int64) error {
		if _, err := r.Seek(offset, io.SeekStart); err != nil {
			return err
		}
		n, err := io.ReadFull(r, buf)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return err
		}
		if n > 0 {
			h.Write(buf[:n])
		}
		return nil
	}

	if size <= sampleChunkSize*3 {
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return "", err
		}
		if _, err := io.Copy(h, r); err != nil {
			return "", err
		}
	} else {
		if err := readSlice(0); err != nil {
			return "", err
		}
		if err := readSlice((size / 2) - (sampleChunkSize / 2)); err != nil {
			return "", err
		}
		if err := readSlice(size - sampleChunkSize); err != nil {
			return "", err
		}
	}

	return hex.EncodeToString(h.Sum(nil)[:16]), nil
}

// handleStat returns size and fast_hash for a file so the Hub can store resilient file pointers.
func (s *AgentServer) handleStat(w http.ResponseWriter, r *http.Request) {
	f, info, cleanup, err := s.openMountFile(r.PathValue("mountId"), r.URL.Query().Get("path"))
	if err != nil {
		writeVFSError(w, err)
		return
	}
	defer cleanup()
	defer f.Close()

	if info.IsDir() {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"is_dir":     true,
			"size_bytes": 0,
			"fast_hash":  "",
		})
		return
	}

	fh, err := ComputeFastHash(f, info.Size())
	if err != nil {
		writeVFSError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"is_dir":     false,
		"size_bytes": info.Size(),
		"fast_hash":  fh,
		"mime_type":  detectMimeByExt(info.Name()),
	})
}

// handleRelocate searches enabled mounts for a moved or renamed file matching (size_bytes, fast_hash).
func (s *AgentServer) handleRelocate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SizeBytes int64  `json:"size_bytes"`
		FastHash  string `json:"fast_hash"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.FastHash == "" {
		http.Error(w, `{"error":"size_bytes and fast_hash are required"}`, http.StatusBadRequest)
		return
	}

	cfg := s.GetConfig()
	if cfg.Paused {
		writeVFSError(w, errPaused)
		return
	}

	for _, m := range cfg.Mounts {
		if !m.Enabled || m.Rejected {
			continue
		}
		resolved, err := ExpandLocalPath(m.LocalPath)
		if err != nil {
			continue
		}

		// Single-file mount check
		if m.Kind == "file" {
			info, err := os.Stat(resolved)
			if err != nil || info.IsDir() || info.Size() != req.SizeBytes {
				continue
			}
			f, err := os.Open(resolved)
			if err != nil {
				continue
			}
			fh, err := ComputeFastHash(f, info.Size())
			_ = f.Close()
			if err == nil && fh == req.FastHash {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"found":    true,
					"mount_id": m.ID,
					"rel_path": filepath.Base(resolved),
				})
				return
			}
			continue
		}

		// Folder mount scan (metadata size-filter first -> fast_hash only on exact size match)
		root, err := os.OpenRoot(resolved)
		if err != nil {
			continue
		}
		foundRel := ""
		_ = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() || foundRel != "" {
				return nil
			}
			fi, err := d.Info()
			if err != nil || fi.Size() != req.SizeBytes {
				return nil
			}
			f, err := root.Open(filepath.FromSlash(p))
			if err != nil {
				return nil
			}
			fh, err := ComputeFastHash(f, fi.Size())
			_ = f.Close()
			if err == nil && fh == req.FastHash {
				foundRel = strings.TrimPrefix(p, "./")
				return fs.SkipAll
			}
			return nil
		})
		_ = root.Close()

		if foundRel != "" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"found":    true,
				"mount_id": m.ID,
				"rel_path": foundRel,
			})
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"found": false})
}

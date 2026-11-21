package vfs

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
)

// handleReveal opens the local file or folder in the machine's native file manager
// (Windows Explorer /select, macOS Finder -R, Linux xdg-open) for the machine's owner.
func (s *AgentServer) handleReveal(w http.ResponseWriter, r *http.Request) {
	m, _, err := s.findMount(r.PathValue("mountId"))
	if err != nil {
		writeVFSError(w, err)
		return
	}
	resolved, err := ExpandLocalPath(m.LocalPath)
	if err != nil {
		writeVFSError(w, err)
		return
	}

	target := resolved
	if m.Kind != "file" {
		rel, err := CleanRelPath(r.URL.Query().Get("path"))
		if err != nil {
			writeVFSError(w, err)
			return
		}
		if rel != "" {
			// Verify the target stays inside the mount root (blocks symlink escapes).
			root, _, err := s.openMountRoot(r.PathValue("mountId"))
			if err != nil {
				writeVFSError(w, err)
				return
			}
			_, statErr := root.Stat(filepath.FromSlash(rel))
			_ = root.Close()
			if statErr != nil {
				writeVFSError(w, statErr)
				return
			}
			target = filepath.Join(resolved, filepath.FromSlash(rel))
		}
	}

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer.exe", "/select,"+target)
	case "darwin":
		cmd = exec.Command("open", "-R", target)
	default:
		cmd = exec.Command("xdg-open", filepath.Dir(target))
	}
	_ = cmd.Start()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "path": target})
}

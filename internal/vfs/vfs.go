package vfs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ourchive/ourchive/pkg/protocol"
)

// TrafficMeter tracks rolling 1-second upload (served) and download (received) byte rates.
type TrafficMeter struct {
	upBytes   atomic.Int64
	downBytes atomic.Int64

	mu         sync.Mutex
	lastSample time.Time
	lastUp     int64
	lastDown   int64
	upBps      int64
	downBps    int64
}

func NewTrafficMeter() *TrafficMeter {
	return &TrafficMeter{lastSample: time.Now()}
}

func (m *TrafficMeter) AddUpload(n int64)   { m.upBytes.Add(n) }
func (m *TrafficMeter) AddDownload(n int64) { m.downBytes.Add(n) }

// Sample returns the recent bytes/sec throughput for upload and download.
func (m *TrafficMeter) Sample() (upBps, downBps int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	dt := now.Sub(m.lastSample).Seconds()
	if dt >= 0.5 {
		curUp := m.upBytes.Load()
		curDown := m.downBytes.Load()
		m.upBps = int64(float64(curUp-m.lastUp) / dt)
		m.downBps = int64(float64(curDown-m.lastDown) / dt)
		m.lastUp = curUp
		m.lastDown = curDown
		m.lastSample = now
	}
	return m.upBps, m.downBps
}

// ConfigSnapshot supplies live Agent settings to the VFS handler.
type ConfigSnapshot struct {
	Paused        bool
	ReadOnly      bool
	UploadCapMbps int
	Mounts        []protocol.FolderMount
}

// AgentServer serves os.Root-jailed folders over incoming yamux HTTP streams.
type AgentServer struct {
	GetConfig func() ConfigSnapshot
	Meter     *TrafficMeter
	mux       *http.ServeMux
}

func NewAgentServer(getConfig func() ConfigSnapshot, meter *TrafficMeter) *AgentServer {
	s := &AgentServer{
		GetConfig: getConfig,
		Meter:     meter,
		mux:       http.NewServeMux(),
	}
	s.mux.HandleFunc("GET /vfs/{mountId}/list", s.handleList)
	s.mux.HandleFunc("GET /vfs/{mountId}/file", s.handleGetFile)
	s.mux.HandleFunc("HEAD /vfs/{mountId}/file", s.handleGetFile)
	s.mux.HandleFunc("GET /vfs/{mountId}/thumb", s.handleThumb)
	s.mux.HandleFunc("GET /vfs/{mountId}/zip", s.handleZip)
	s.mux.HandleFunc("GET /vfs/{mountId}/stat", s.handleStat)
	s.mux.HandleFunc("POST /vfs/relocate", s.handleRelocate)
	s.mux.HandleFunc("PUT /vfs/{mountId}/file", s.handlePutFile)
	s.mux.HandleFunc("POST /vfs/{mountId}/mkdir", s.handleMkdir)
	s.mux.HandleFunc("POST /vfs/{mountId}/rename", s.handleRename)
	s.mux.HandleFunc("DELETE /vfs/{mountId}/entry", s.handleDelete)
	s.mux.HandleFunc("GET /vfs/search", s.handleSearch)
	s.mux.HandleFunc("POST /vfs/pick-folder", s.handlePickFolder)
	s.mux.HandleFunc("POST /vfs/pick-file", s.handlePickFile)
	s.mux.HandleFunc("POST /vfs/{mountId}/reveal", s.handleReveal)
	s.mux.HandleFunc("POST /p2p/offer", s.handleP2POffer)
	return s
}

func (s *AgentServer) handlePickFolder(w http.ResponseWriter, r *http.Request) {
	picked, err := PickFolderNative(r.Context())
	if errors.Is(err, ErrPickerCancelled) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"cancelled": true})
		return
	}
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"path": picked,
		"name": filepath.Base(picked),
		"kind": "folder",
	})
}

func (s *AgentServer) handlePickFile(w http.ResponseWriter, r *http.Request) {
	picked, err := PickFileNative(r.Context())
	if errors.Is(err, ErrPickerCancelled) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"cancelled": true})
		return
	}
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"path": picked,
		"name": filepath.Base(picked),
		"kind": "file",
	})
}

func (s *AgentServer) Handle(pattern string, handler http.Handler) {
	s.mux.Handle(pattern, handler)
}

func (s *AgentServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// CleanRelPath normalizes a user-supplied relative path into a safe, leading-slash-free path.
func CleanRelPath(raw string) (string, error) {
	raw = strings.ReplaceAll(raw, "\\", "/")
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "/" || raw == "." {
		return "", nil
	}
	cleaned := path.Clean("/" + raw)
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "." {
		return "", nil
	}
	if strings.HasPrefix(cleaned, "..") {
		return "", errors.New("path escapes mount root")
	}
	return cleaned, nil
}

func (s *AgentServer) findMount(mountID string) (protocol.FolderMount, ConfigSnapshot, error) {
	cfg := s.GetConfig()
	if cfg.Paused {
		return protocol.FolderMount{}, cfg, errPaused
	}
	for _, m := range cfg.Mounts {
		if m.ID == mountID {
			if !m.Enabled || m.Rejected {
				return protocol.FolderMount{}, cfg, errMountDisabled
			}
			return m, cfg, nil
		}
	}
	return protocol.FolderMount{}, cfg, os.ErrNotExist
}

func (s *AgentServer) openMountRoot(mountID string) (*os.Root, ConfigSnapshot, error) {
	m, cfg, err := s.findMount(mountID)
	if err != nil {
		return nil, cfg, err
	}
	if m.Kind == "file" {
		return nil, cfg, errors.New("mount is a single file reference, not a folder")
	}
	resolved, err := ExpandLocalPath(m.LocalPath)
	if err != nil {
		return nil, cfg, err
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, cfg, err
	}
	return root, cfg, nil
}

// openMountFile opens a file from either a folder mount or a single-file reference mount,
// always jailed via os.OpenRoot so sibling files can never be accessed.
func (s *AgentServer) openMountFile(mountID, rawRel string) (*os.File, os.FileInfo, func(), error) {
	m, _, err := s.findMount(mountID)
	if err != nil {
		return nil, nil, nil, err
	}
	resolved, err := ExpandLocalPath(m.LocalPath)
	if err != nil {
		return nil, nil, nil, err
	}
	if m.Kind == "file" {
		root, err := os.OpenRoot(filepath.Dir(resolved))
		if err != nil {
			return nil, nil, nil, err
		}
		f, err := root.Open(filepath.Base(resolved))
		if err != nil {
			_ = root.Close()
			return nil, nil, nil, err
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			_ = root.Close()
			return nil, nil, nil, err
		}
		return f, info, func() { _ = root.Close() }, nil
	}

	rel, err := CleanRelPath(rawRel)
	if err != nil || rel == "" {
		return nil, nil, nil, errors.New("valid file path is required")
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, nil, nil, err
	}
	f, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		_ = root.Close()
		return nil, nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		_ = root.Close()
		return nil, nil, nil, err
	}
	return f, info, func() { _ = root.Close() }, nil
}

var (
	errPaused        = errors.New("node sharing is paused")
	errMountDisabled = errors.New("folder mount is disabled or unavailable")
)

func writeVFSError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errPaused):
		http.Error(w, `{"error":"sharing is paused on this machine"}`, http.StatusServiceUnavailable)
	case errors.Is(err, errMountDisabled):
		http.Error(w, `{"error":"folder is disabled or unavailable"}`, http.StatusForbidden)
	case errors.Is(err, os.ErrNotExist):
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	default:
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
	}
}

func (s *AgentServer) handleList(w http.ResponseWriter, r *http.Request) {
	m, _, err := s.findMount(r.PathValue("mountId"))
	if err != nil {
		writeVFSError(w, err)
		return
	}

	// Single-file reference mount: expose only that exact file (zero sibling visibility).
	if m.Kind == "file" {
		resolved, err := ExpandLocalPath(m.LocalPath)
		if err != nil {
			writeVFSError(w, err)
			return
		}
		info, err := os.Stat(resolved)
		if err != nil {
			writeVFSError(w, err)
			return
		}
		base := filepath.Base(resolved)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.ListResponse{
			Path: "",
			Entries: []protocol.FileEntry{
				{
					Name:     base,
					Path:     base,
					IsDir:    false,
					Size:     info.Size(),
					ModTime:  info.ModTime().UTC().Format(time.RFC3339),
					MimeType: detectMimeByExt(base),
				},
			},
		})
		return
	}

	root, _, err := s.openMountRoot(r.PathValue("mountId"))
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
	if rel != "" {
		target = filepath.FromSlash(rel)
	}

	f, err := root.Open(target)
	if err != nil {
		writeVFSError(w, err)
		return
	}
	defer f.Close()

	dirEntries, err := f.ReadDir(-1)
	if err != nil {
		writeVFSError(w, err)
		return
	}

	entries := make([]protocol.FileEntry, 0, len(dirEntries))
	for _, de := range dirEntries {
		name := de.Name()
		if strings.HasSuffix(name, partFileSuffix) {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		entryPath := name
		if rel != "" {
			entryPath = rel + "/" + name
		}
		mimeType := "inode/directory"
		if !de.IsDir() {
			mimeType = detectMimeByExt(name)
		}
		entries = append(entries, protocol.FileEntry{
			Name:     name,
			Path:     entryPath,
			IsDir:    de.IsDir(),
			Size:     info.Size(),
			ModTime:  info.ModTime().UTC().Format(time.RFC3339),
			MimeType: mimeType,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(protocol.ListResponse{
		Path:    rel,
		Entries: entries,
	})
}

func (s *AgentServer) handleGetFile(w http.ResponseWriter, r *http.Request) {
	_, cfg, err := s.findMount(r.PathValue("mountId"))
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
	defer f.Close()

	if info.IsDir() {
		http.Error(w, `{"error":"path is not a file"}`, http.StatusBadRequest)
		return
	}

	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, info.Name()))
	}
	w.Header().Set("Content-Type", detectMimeByExt(info.Name()))
	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, fileETag(info)))
	w.Header().Set("Accept-Ranges", "bytes")

	var dst http.ResponseWriter = w
	if s.Meter != nil || cfg.UploadCapMbps > 0 {
		dst = &meteredWriter{
			ResponseWriter: w,
			meter:          s.Meter,
			capBytesPerSec: int64(cfg.UploadCapMbps) * 125_000, // Mbps -> Bytes/sec
			startTime:      time.Now(),
		}
	}

	http.ServeContent(dst, r, info.Name(), info.ModTime(), f)
}

func (s *AgentServer) handlePutFile(w http.ResponseWriter, r *http.Request) {
	if s.GetConfig().ReadOnly {
		http.Error(w, `{"error":"agent is running in read-only mode"}`, http.StatusForbidden)
		return
	}
	root, _, err := s.openMountRoot(r.PathValue("mountId"))
	if err != nil {
		writeVFSError(w, err)
		return
	}
	defer root.Close()

	rel, err := CleanRelPath(r.URL.Query().Get("path"))
	if err != nil || rel == "" {
		http.Error(w, `{"error":"valid file path is required"}`, http.StatusBadRequest)
		return
	}

	if dir := path.Dir(rel); dir != "." && dir != "" {
		if err := root.MkdirAll(filepath.FromSlash(dir), 0o755); err != nil {
			writeVFSError(w, err)
			return
		}
	}

	partRel := rel + partFileSuffix
	partOS := filepath.FromSlash(partRel)
	resumeMode := r.URL.Query().Get("resume") == "1"
	isFinal := r.URL.Query().Get("final") != "0"

	var f *os.File
	var existingBytes int64
	if resumeMode {
		if info, statErr := root.Stat(partOS); statErr == nil && !info.IsDir() {
			if opened, openErr := root.OpenFile(partOS, os.O_WRONLY|os.O_APPEND, 0o644); openErr == nil {
				f = opened
				existingBytes = info.Size()
			}
		}
	}
	if f == nil {
		created, createErr := root.Create(partOS)
		if createErr != nil {
			writeVFSError(w, createErr)
			return
		}
		f = created
	}

	written, copyErr := io.Copy(f, r.Body)
	syncErr := f.Sync()
	closeErr := f.Close()
	if copyErr != nil {
		// Leave .ourchive.part on disk so the client can safely resume!
		writeVFSError(w, copyErr)
		return
	}
	if syncErr != nil {
		writeVFSError(w, syncErr)
		return
	}
	if closeErr != nil {
		writeVFSError(w, closeErr)
		return
	}

	if isFinal {
		if err := root.Rename(partOS, filepath.FromSlash(rel)); err != nil {
			writeVFSError(w, err)
			return
		}
	}

	if s.Meter != nil {
		s.Meter.AddDownload(written)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"path":    rel,
		"bytes":   existingBytes + written,
		"resumed": resumeMode && existingBytes > 0,
		"final":   isFinal,
	})
}

func (s *AgentServer) handleMkdir(w http.ResponseWriter, r *http.Request) {
	if s.GetConfig().ReadOnly {
		http.Error(w, `{"error":"agent is running in read-only mode"}`, http.StatusForbidden)
		return
	}
	root, _, err := s.openMountRoot(r.PathValue("mountId"))
	if err != nil {
		writeVFSError(w, err)
		return
	}
	defer root.Close()

	rel, err := CleanRelPath(r.URL.Query().Get("path"))
	if err != nil || rel == "" {
		http.Error(w, `{"error":"folder path is required"}`, http.StatusBadRequest)
		return
	}

	if err := root.MkdirAll(filepath.FromSlash(rel), 0o755); err != nil {
		writeVFSError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"path": rel})
}

func (s *AgentServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	if s.GetConfig().ReadOnly {
		http.Error(w, `{"error":"agent is running in read-only mode"}`, http.StatusForbidden)
		return
	}
	root, _, err := s.openMountRoot(r.PathValue("mountId"))
	if err != nil {
		writeVFSError(w, err)
		return
	}
	defer root.Close()

	rel, err := CleanRelPath(r.URL.Query().Get("path"))
	if err != nil || rel == "" {
		http.Error(w, `{"error":"cannot delete mount root"}`, http.StatusBadRequest)
		return
	}

	if err := root.RemoveAll(filepath.FromSlash(rel)); err != nil {
		writeVFSError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *AgentServer) handleRename(w http.ResponseWriter, r *http.Request) {
	if s.GetConfig().ReadOnly {
		http.Error(w, `{"error":"agent is running in read-only mode"}`, http.StatusForbidden)
		return
	}
	root, _, err := s.openMountRoot(r.PathValue("mountId"))
	if err != nil {
		writeVFSError(w, err)
		return
	}
	defer root.Close()

	var req struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	fromRel, err := CleanRelPath(req.From)
	if err != nil || fromRel == "" {
		http.Error(w, `{"error":"valid source path is required"}`, http.StatusBadRequest)
		return
	}
	toRel, err := CleanRelPath(req.To)
	if err != nil || toRel == "" {
		http.Error(w, `{"error":"valid destination path is required"}`, http.StatusBadRequest)
		return
	}
	if dir := path.Dir(toRel); dir != "." && dir != "" {
		if err := root.MkdirAll(filepath.FromSlash(dir), 0o755); err != nil {
			writeVFSError(w, err)
			return
		}
	}
	if err := root.Rename(filepath.FromSlash(fromRel), filepath.FromSlash(toRel)); err != nil {
		writeVFSError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "from": fromRel, "to": toRel})
}

type SearchHit struct {
	MountID   string `json:"mount_id"`
	MountName string `json:"mount_name"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	IsDir     bool   `json:"is_dir"`
	Size      int64  `json:"size"`
	ModTime   string `json:"mod_time"`
	MimeType  string `json:"mime_type"`
}

func (s *AgentServer) handleSearch(w http.ResponseWriter, r *http.Request) {
	cfg := s.GetConfig()
	if cfg.Paused {
		writeVFSError(w, errPaused)
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if q == "" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"hits": []SearchHit{}})
		return
	}
	limit := 80
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 250 {
			limit = n
		}
	}

	hits := make([]SearchHit, 0, 32)
	for _, m := range cfg.Mounts {
		if len(hits) >= limit {
			break
		}
		if !m.Enabled || m.Rejected {
			continue
		}
		resolved, err := ExpandLocalPath(m.LocalPath)
		if err != nil {
			continue
		}
		if m.Kind == "file" {
			base := filepath.Base(resolved)
			if strings.Contains(strings.ToLower(base), q) || strings.Contains(strings.ToLower(m.Name), q) {
				if info, err := os.Stat(resolved); err == nil {
					hits = append(hits, SearchHit{
						MountID:   m.ID,
						MountName: m.Name,
						Name:      base,
						Path:      base,
						IsDir:     false,
						Size:      info.Size(),
						ModTime:   info.ModTime().UTC().Format(time.RFC3339),
						MimeType:  detectMimeByExt(base),
					})
				}
			}
			continue
		}

		root, err := os.OpenRoot(resolved)
		if err != nil {
			continue
		}
		_ = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil || p == "." {
				return nil
			}
			if len(hits) >= limit {
				return fs.SkipAll
			}
			if !strings.Contains(strings.ToLower(d.Name()), q) {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			mimeType := "inode/directory"
			if !d.IsDir() {
				mimeType = detectMimeByExt(d.Name())
			}
			hits = append(hits, SearchHit{
				MountID:   m.ID,
				MountName: m.Name,
				Name:      d.Name(),
				Path:      p,
				IsDir:     d.IsDir(),
				Size:      info.Size(),
				ModTime:   info.ModTime().UTC().Format(time.RFC3339),
				MimeType:  mimeType,
			})
			return nil
		})
		_ = root.Close()
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"hits": hits})
}

type meteredWriter struct {
	http.ResponseWriter
	meter          *TrafficMeter
	capBytesPerSec int64
	startTime      time.Time
	sent           int64
}

func (mw *meteredWriter) Write(p []byte) (int, error) {
	n, err := mw.ResponseWriter.Write(p)
	if n > 0 {
		mw.sent += int64(n)
		if mw.meter != nil {
			mw.meter.AddUpload(int64(n))
		}
		if mw.capBytesPerSec > 0 {
			expected := time.Duration(float64(mw.sent) / float64(mw.capBytesPerSec) * float64(time.Second))
			if wait := expected - time.Since(mw.startTime); wait > 0 {
				time.Sleep(wait)
			}
		}
	}
	return n, err
}

func detectMimeByExt(name string) string {
	ext := strings.ToLower(path.Ext(name))
	switch ext {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mkv":
		return "video/x-matroska"
	case ".mov":
		return "video/quicktime"
	case ".mp3":
		return "audio/mpeg"
	case ".flac":
		return "audio/flac"
	case ".wav":
		return "audio/wav"
	case ".ogg", ".opus":
		return "audio/ogg"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".svg":
		return "image/svg+xml"
	case ".pdf":
		return "application/pdf"
	case ".md":
		return "text/markdown"
	case ".json":
		return "application/json"
	case ".txt", ".log", ".go", ".ts", ".tsx", ".js", ".css", ".html", ".sql":
		return "text/plain; charset=utf-8"
	}
	if m := mime.TypeByExtension(ext); m != "" {
		return m
	}
	return "application/octet-stream"
}

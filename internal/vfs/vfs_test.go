package vfs

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ourchive/ourchive/internal/auth"
	"github.com/ourchive/ourchive/internal/db"
	"github.com/ourchive/ourchive/pkg/protocol"
)

func TestAgentServerVFSAndRangeAndSecurity(t *testing.T) {
	mountDir := t.TempDir()
	secretFile := filepath.Join(filepath.Dir(mountDir), "outside-secret.txt")
	_ = os.WriteFile(secretFile, []byte("top-secret"), 0o600)

	meter := NewTrafficMeter()
	srv := NewAgentServer(func() ConfigSnapshot {
		return ConfigSnapshot{
			Paused: false,
			Mounts: []protocol.FolderMount{
				{ID: "m1", Name: "Media", LocalPath: mountDir, Enabled: true},
			},
		}
	}, meter)

	// 1. MKDIR
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/vfs/m1/mkdir?path=2026/clips", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("mkdir expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// 2. PUT file
	payload := []byte("0123456789abcdef")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/vfs/m1/file?path=2026/clips/sample.txt", bytes.NewReader(payload)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("put expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// 3. LIST directory
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vfs/m1/list?path=2026/clips", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var listRes protocol.ListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listRes); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listRes.Entries) != 1 || listRes.Entries[0].Name != "sample.txt" || listRes.Entries[0].Size != 16 {
		t.Fatalf("unexpected list entries: %+v", listRes.Entries)
	}

	// 4. GET with HTTP Range
	req := httptest.NewRequest(http.MethodGet, "/vfs/m1/file?path=2026/clips/sample.txt", nil)
	req.Header.Set("Range", "bytes=4-9")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("range expected 206, got %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if string(body) != "456789" {
		t.Fatalf("expected range body '456789', got %q", string(body))
	}

	// 5. Path traversal attempt must not escape os.Root
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vfs/m1/file?path=../outside-secret.txt", nil))
	if rec.Code == http.StatusOK {
		t.Fatalf("path traversal unexpectedly succeeded!")
	}

	// 6. DELETE file
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/vfs/m1/entry?path=2026/clips/sample.txt", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete expected 200, got %d", rec.Code)
	}
}

func TestTagComposedSharedTrees(t *testing.T) {
	ctx := context.Background()
	store, err := db.OpenSQLite(ctx, filepath.Join(t.TempDir(), "tags.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer store.Close()

	authSvc := auth.NewService(store)
	u, err := authSvc.SetupHostAdmin(ctx, "ru@ourchive.local", "ru", "Ru", "password123")
	if err != nil {
		t.Fatalf("SetupHostAdmin: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = store.DB.ExecContext(ctx,
		`INSERT INTO nodes (id, owner_id, display_name, created_at) VALUES ('n1', ?, 'songbird', ?)`, u.ID, now)
	if err != nil {
		t.Fatalf("insert node: %v", err)
	}
	_, err = store.DB.ExecContext(ctx,
		`INSERT INTO mounts (id, node_id, name, local_path, enabled, created_at) VALUES ('m1', 'n1', 'Clips', 'C:\Clips', 1, ?)`, now)
	if err != nil {
		t.Fatalf("insert mount: %v", err)
	}

	ts := NewTagStore(store)
	if _, err := ts.AttachTag(ctx, "2026 Clips", "n1", "m1", "raid/boss.mp4", false, u.ID); err != nil {
		t.Fatalf("AttachTag: %v", err)
	}

	byMount, err := ts.TagsByMount(ctx, "m1")
	if err != nil || len(byMount["raid/boss.mp4"]) != 1 || byMount["raid/boss.mp4"][0] != "2026-clips" {
		t.Fatalf("unexpected TagsByMount: %+v (%v)", byMount, err)
	}

	items, err := ts.ListTreeItems(ctx, "2026-clips")
	if err != nil || len(items) != 1 || items[0].NodeName != "songbird" || items[0].MountName != "Clips" {
		t.Fatalf("unexpected ListTreeItems: %+v (%v)", items, err)
	}

	ts.CleanupDeletedPath(ctx, "m1", "raid")
	itemsAfter, _ := ts.ListTreeItems(ctx, "2026-clips")
	if len(itemsAfter) != 0 {
		t.Fatalf("expected tagged item to be cleaned up on parent dir delete, got %d", len(itemsAfter))
	}
}

func TestExpandLocalPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}

	cases := []struct {
		in   string
		want string
	}{
		{"~", filepath.Clean(home)},
		{"~/Videos", filepath.Join(home, "Videos")},
		{`~\Downloads`, filepath.Join(home, "Downloads")},
		{"$HOME/Clips", filepath.Join(home, "Clips")},
		{`%USERPROFILE%\Pictures`, filepath.Join(home, "Pictures")},
		{`"~/Documents"`, filepath.Join(home, "Documents")},
	}

	for _, tc := range cases {
		got, err := ExpandLocalPath(tc.in)
		if err != nil {
			t.Fatalf("ExpandLocalPath(%q) error: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ExpandLocalPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAgentThumbNonMediaReturnsNoContent(t *testing.T) {
	mountDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(mountDir, "notes.txt"), []byte("hello"), 0o644)

	srv := NewAgentServer(func() ConfigSnapshot {
		return ConfigSnapshot{
			Mounts: []protocol.FolderMount{{ID: "m1", Name: "Docs", LocalPath: mountDir, Enabled: true}},
		}
	}, nil)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vfs/m1/thumb?path=notes.txt", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for non-media thumb, got %d", rec.Code)
	}
}

func TestSingleFileMountAndAutoRelocate(t *testing.T) {
	dir := t.TempDir()
	singleFile := filepath.Join(dir, "clip.bin")
	siblingFile := filepath.Join(dir, "private-sibling.bin")
	content := bytes.Repeat([]byte("ourchive-pointer-test-"), 200)
	_ = os.WriteFile(singleFile, content, 0o644)
	_ = os.WriteFile(siblingFile, []byte("do not expose"), 0o644)

	// 1. Single-file mount exposes ONLY clip.bin, never private-sibling.bin
	srv := NewAgentServer(func() ConfigSnapshot {
		return ConfigSnapshot{
			Mounts: []protocol.FolderMount{
				{ID: "f1", Name: "Clip", LocalPath: singleFile, Kind: "file", Enabled: true},
			},
		}
	}, nil)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vfs/f1/list", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("single-file list expected 200, got %d", rec.Code)
	}
	var listRes protocol.ListResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &listRes)
	if len(listRes.Entries) != 1 || listRes.Entries[0].Name != "clip.bin" {
		t.Fatalf("expected only clip.bin, got %+v", listRes.Entries)
	}

	// 2. Stat returns fast_hash; move & rename the file into a subfolder; relocate finds it!
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vfs/f1/stat?path=clip.bin", nil))
	var st struct {
		SizeBytes int64  `json:"size_bytes"`
		FastHash  string `json:"fast_hash"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if st.FastHash == "" || st.SizeBytes != int64(len(content)) {
		t.Fatalf("unexpected stat: %+v", st)
	}

	archiveDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(archiveDir, "moved", "2026"), 0o755)
	_ = os.WriteFile(filepath.Join(archiveDir, "moved", "2026", "renamed-clip.bin"), content, 0o644)

	folderSrv := NewAgentServer(func() ConfigSnapshot {
		return ConfigSnapshot{
			Mounts: []protocol.FolderMount{
				{ID: "m1", Name: "Archive", LocalPath: archiveDir, Kind: "folder", Enabled: true},
			},
		}
	}, nil)

	relocBody, _ := json.Marshal(map[string]any{"size_bytes": st.SizeBytes, "fast_hash": st.FastHash})
	rec = httptest.NewRecorder()
	folderSrv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/vfs/relocate", bytes.NewReader(relocBody)))
	var reloc struct {
		Found   bool   `json:"found"`
		MountID string `json:"mount_id"`
		RelPath string `json:"rel_path"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &reloc)
	if !reloc.Found || reloc.MountID != "m1" || reloc.RelPath != "moved/2026/renamed-clip.bin" {
		t.Fatalf("relocate failed: %+v", reloc)
	}
}

package shares

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ourchive/ourchive/internal/auth"
	"github.com/ourchive/ourchive/internal/db"
	"github.com/ourchive/ourchive/internal/tunnel"
	"github.com/ourchive/ourchive/internal/vfs"
	"github.com/ourchive/ourchive/pkg/protocol"
)

func TestShareLinksEmbedPasswordAndZipStreaming(t *testing.T) {
	ctx := context.Background()
	store, err := db.OpenSQLite(ctx, filepath.Join(t.TempDir(), "shares.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer store.Close()

	authSvc := auth.NewService(store)
	u, err := authSvc.SetupHostAdmin(ctx, "ru@ourchive.local", "Ru", "password123")
	if err != nil {
		t.Fatalf("SetupHostAdmin: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, _ = store.DB.ExecContext(ctx, `INSERT INTO nodes (id, owner_id, display_name, created_at) VALUES ('n1', ?, 'songbird', ?)`, u.ID, now)
	_, _ = store.DB.ExecContext(ctx, `INSERT INTO mounts (id, node_id, name, local_path, enabled, created_at) VALUES ('m1', 'n1', 'Clips', 'C:\Clips', 1, ?)`, now)

	broker := tunnel.NewBroker(store)
	svc := NewService(store, broker, nil, "http://s.localhost:8080", t.TempDir())
	handler := svc.ShareHandler()

	// 1. Create public video share link -> verify OpenGraph & oEmbed tags
	link, err := svc.Create(ctx, CreateRequest{
		NodeID:    "n1",
		MountID:   "m1",
		RelPath:   "raid/boss.mp4",
		Title:     "Boss Kill",
		MimeType:  "video/mp4",
		SizeBytes: 5_000_000,
	}, u.ID)
	if err != nil {
		t.Fatalf("Create share: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://s.localhost:8080/"+link.Slug, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	htmlBody := rec.Body.String()
	if !strings.Contains(htmlBody, `property="og:video"`) || !strings.Contains(htmlBody, `application/json+oembed`) {
		t.Fatalf("expected OpenGraph video & oEmbed tags in HTML")
	}
	if rec.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("expected X-Robots-Tag noindex, nofollow")
	}

	// 2. Password-protected share link -> must prompt before showing media
	pwLink, err := svc.Create(ctx, CreateRequest{
		NodeID:   "n1",
		MountID:  "m1",
		RelPath:  "secret.png",
		Title:    "Secret",
		MimeType: "image/png",
		Password: "letmein99",
		MaxViews: 2,
	}, u.ID)
	if err != nil {
		t.Fatalf("Create password share: %v", err)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://s.localhost:8080/"+pwLink.Slug, nil))
	if !strings.Contains(rec.Body.String(), "password protected") {
		t.Fatalf("expected password prompt for locked share")
	}

	// Unlock with valid password
	form := url.Values{"password": {"letmein99"}}
	unlockReq := httptest.NewRequest(http.MethodPost, "http://s.localhost:8080/"+pwLink.Slug+"/unlock", strings.NewReader(form.Encode()))
	unlockReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, unlockReq)
	if rec.Code != http.StatusSeeOther || len(rec.Result().Cookies()) == 0 {
		t.Fatalf("expected redirect + unlock cookie, got %d", rec.Code)
	}

	// 3. Verify Agent on-the-fly streaming ZIP produces a valid readable ZIP
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "bundle", "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "bundle", "readme.txt"), []byte("hello zip"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "bundle", "sub", "data.txt"), []byte("nested"), 0o644)

	vfsSrv := vfs.NewAgentServer(func() vfs.ConfigSnapshot {
		return vfs.ConfigSnapshot{
			Mounts: []protocol.FolderMount{{ID: "m1", Name: "Docs", LocalPath: dir, Enabled: true}},
		}
	}, nil)
	zipRec := httptest.NewRecorder()
	vfsSrv.ServeHTTP(zipRec, httptest.NewRequest(http.MethodGet, "/vfs/m1/zip?path=bundle", nil))
	if zipRec.Code != http.StatusOK {
		t.Fatalf("zip expected 200, got %d", zipRec.Code)
	}
	zr, err := zip.NewReader(bytes.NewReader(zipRec.Body.Bytes()), int64(zipRec.Body.Len()))
	if err != nil {
		t.Fatalf("invalid zip output: %v", err)
	}
	if len(zr.File) != 2 {
		t.Fatalf("expected 2 files in zip, got %d", len(zr.File))
	}
}

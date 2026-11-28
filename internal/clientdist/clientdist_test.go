package clientdist

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

const repo = "kevinaliyev/ourchive"

func rel(tag string, draft, pre bool, assets map[string]string) map[string]any {
	as := []map[string]any{}
	for name, url := range assets {
		as = append(as, map[string]any{"name": name, "size": 1234, "browser_download_url": url})
	}
	return map[string]any{
		"tag_name": tag, "draft": draft, "prerelease": pre,
		"html_url": "https://github.com/" + repo + "/releases/tag/" + tag, "assets": as,
	}
}

func dl(tag, name string) string {
	return "https://github.com/" + repo + "/releases/download/" + tag + "/" + name
}

type fakeGitHub struct {
	srv      *httptest.Server
	calls    atomic.Int32
	status   atomic.Int32
	releases atomic.Value // []map[string]any
}

func newFakeGitHub(t *testing.T, releases []map[string]any) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{}
	f.status.Store(200)
	f.releases.Store(releases)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if r.URL.Path != "/repos/"+repo+"/releases" {
			http.NotFound(w, r)
			return
		}
		if s := int(f.status.Load()); s != 200 {
			http.Error(w, "nope", s)
			return
		}
		_ = json.NewEncoder(w).Encode(f.releases.Load())
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestReleasePicksNewestPublishedClientRelease(t *testing.T) {
	f := newFakeGitHub(t, []map[string]any{
		rel("client-v0.3.0", true, false, map[string]string{"ourchive-client-windows-amd64.exe": dl("client-v0.3.0", "ourchive-client-windows-amd64.exe")}),
		rel("client-v0.3.0-rc1", false, true, map[string]string{"ourchive-client-windows-amd64.exe": dl("client-v0.3.0-rc1", "ourchive-client-windows-amd64.exe")}),
		rel("v1.9.0", false, false, map[string]string{"ourchive-agent-windows-amd64.exe": dl("v1.9.0", "ourchive-agent-windows-amd64.exe")}),
		rel("client-v0.2.0", false, false, map[string]string{
			"ourchive-client-windows-amd64.exe":    dl("client-v0.2.0", "ourchive-client-windows-amd64.exe"),
			"ourchive-client-linux-amd64.AppImage": dl("client-v0.2.0", "ourchive-client-linux-amd64.AppImage"),
			"notes.txt":                            dl("client-v0.2.0", "notes.txt"),
		}),
		rel("client-v0.1.0", false, false, map[string]string{"ourchive-client-windows-amd64.exe": dl("client-v0.1.0", "ourchive-client-windows-amd64.exe")}),
	})
	info := New(Options{Repo: repo, APIBase: f.srv.URL}).Resolve(context.Background())

	if info.Source != SourceRelease || info.Version != "v0.2.0" {
		t.Fatalf("source/version = %q/%q, want release/v0.2.0", info.Source, info.Version)
	}
	if len(info.Assets) != 2 || info.Assets[0].OS != "windows" || info.Assets[1].OS != "linux" {
		t.Fatalf("assets = %+v, want windows then linux only", info.Assets)
	}
	if info.Assets[0].URL != dl("client-v0.2.0", "ourchive-client-windows-amd64.exe") {
		t.Fatalf("unexpected url %q", info.Assets[0].URL)
	}
	if info.ReleaseURL == "" {
		t.Fatal("release url should be set")
	}
}

func TestReleaseRejectsForeignDownloadURLs(t *testing.T) {
	f := newFakeGitHub(t, []map[string]any{
		rel("client-v0.2.0", false, false, map[string]string{
			"ourchive-client-windows-amd64.exe":    "https://evil.example/ourchive-client-windows-amd64.exe",
			"ourchive-client-linux-amd64.AppImage": "https://github.com/someone/else/releases/download/x/ourchive-client-linux-amd64.AppImage",
			"ourchive-client-macos-arm64.zip":      dl("client-v0.2.0", "ourchive-client-macos-arm64.zip"),
		}),
	})
	info := New(Options{Repo: repo, APIBase: f.srv.URL}).Resolve(context.Background())
	if len(info.Assets) != 1 || info.Assets[0].OS != "macos" {
		t.Fatalf("only the in-repo asset may be offered, got %+v", info.Assets)
	}
}

func TestReleaseIsCachedAndStaleOnFailure(t *testing.T) {
	f := newFakeGitHub(t, []map[string]any{
		rel("client-v0.2.0", false, false, map[string]string{"ourchive-client-windows-amd64.exe": dl("client-v0.2.0", "ourchive-client-windows-amd64.exe")}),
	})
	now := time.Unix(1_700_000_000, 0)
	r := New(Options{Repo: repo, APIBase: f.srv.URL, TTL: 10 * time.Minute, Now: func() time.Time { return now }})
	ctx := context.Background()

	r.Resolve(ctx)
	now = now.Add(5 * time.Minute)
	r.Resolve(ctx)
	if f.calls.Load() != 1 {
		t.Fatalf("lookups inside the TTL must be served from cache, got %d calls", f.calls.Load())
	}

	// After the TTL GitHub fails: keep offering the last good answer, and don't hammer it.
	f.status.Store(500)
	now = now.Add(10 * time.Minute)
	for i := 0; i < 3; i++ {
		info := r.Resolve(ctx)
		if info.Source != SourceRelease || len(info.Assets) != 1 {
			t.Fatalf("stale answer expected while GitHub fails, got %+v", info)
		}
	}
	if f.calls.Load() != 2 {
		t.Fatalf("a failed lookup should be remembered for a minute, got %d calls", f.calls.Load())
	}

	// Recovery picks up the new release.
	f.status.Store(200)
	f.releases.Store([]map[string]any{
		rel("client-v0.3.0", false, false, map[string]string{"ourchive-client-windows-amd64.exe": dl("client-v0.3.0", "ourchive-client-windows-amd64.exe")}),
	})
	now = now.Add(2 * time.Minute)
	if v := r.Resolve(ctx).Version; v != "v0.3.0" {
		t.Fatalf("version after recovery = %q, want v0.3.0", v)
	}
}

func TestFallsBackToHubInstallersThenNone(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Nothing configured and nothing on disk.
	info := New(Options{DistDir: dir}).Resolve(ctx)
	if info.Source != SourceNone || info.Assets == nil || len(info.Assets) != 0 {
		t.Fatalf("want none with an empty (non-nil) list, got %+v", info)
	}

	// Installers on disk are offered from this hub; unrelated and misnamed files are not.
	for _, name := range []string{"ourchive-client-windows-amd64.exe", "ourchive-client-weird.exe", "ourchive-agent-windows-amd64.exe"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "ourchive-client-linux-amd64.AppImage"), 0o755); err != nil {
		t.Fatal(err)
	}
	info = New(Options{DistDir: dir}).Resolve(ctx)
	if info.Source != SourceHub || len(info.Assets) != 1 {
		t.Fatalf("want one hub asset, got %+v", info)
	}
	if a := info.Assets[0]; a.OS != "windows" || a.URL != "/downloads/ourchive-client-windows-amd64.exe" || a.Size != 1 {
		t.Fatalf("unexpected hub asset %+v", a)
	}
}

func TestReleaseWithoutClientReleaseOrOnErrorFallsBackToHub(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ourchive-client-linux-amd64.AppImage"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newFakeGitHub(t, []map[string]any{rel("v1.0.0", false, false, nil)})
	r := New(Options{Repo: repo, DistDir: dir, APIBase: f.srv.URL})
	if info := r.Resolve(context.Background()); info.Source != SourceHub || len(info.Assets) != 1 {
		t.Fatalf("no client release should fall back to the hub, got %+v", info)
	}

	f.status.Store(403)
	r = New(Options{Repo: repo, DistDir: dir, APIBase: f.srv.URL})
	if info := r.Resolve(context.Background()); info.Source != SourceHub {
		t.Fatalf("a GitHub error should fall back to the hub, got %+v", info)
	}
}

func TestValidRepo(t *testing.T) {
	for _, ok := range []string{"kevinaliyev/ourchive", "a/b", "my-org/my.repo_2"} {
		if !ValidRepo(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "ourchive", "a/b/c", "a b/c", "../x", "a/../b", "https://github.com/a/b", "a/b?x=1"} {
		if ValidRepo(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
	// An invalid repo must disable the release source rather than build a request from it.
	if r := New(Options{Repo: "a/b?x=1"}); r.repo != "" {
		t.Fatal("invalid repo should be ignored")
	}
}

// Package clientdist works out where a user can download the Ourchive desktop app.
//
// Two sources are supported, tried in this order:
//
//  1. "release": the newest published client release (tag client-v*) of a GitHub repository, set
//     with OURCHIVE_CLIENT_RELEASE_REPO=owner/repo. The hub asks the GitHub API and caches the
//     answer.
//  2. "hub": installers dropped into the hub's OURCHIVE_DIST_DIR with the stable file names below.
//     The hub serves them from /downloads/.
//
// Installers are matched by exact file name, and release URLs must live under the configured
// repository's releases, so a release can't point the web UI at an unrelated download.
package clientdist

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Platform is one installer the release workflow produces. Filename is stable across versions so
// the hub never has to guess.
type Platform struct {
	OS       string
	Arch     string
	Label    string
	Filename string
}

// Platforms lists every installer the client release workflow publishes.
var Platforms = []Platform{
	{OS: "windows", Arch: "amd64", Label: "Windows", Filename: "ourchive-client-windows-amd64.exe"},
	{OS: "macos", Arch: "arm64", Label: "macOS (Apple silicon)", Filename: "ourchive-client-macos-arm64.zip"},
	{OS: "linux", Arch: "amd64", Label: "Linux", Filename: "ourchive-client-linux-amd64.AppImage"},
}

// FilePrefix is the prefix of every installer name; the hub's /downloads route allows it.
const FilePrefix = "ourchive-client-"

// TagPrefix marks client releases among a repository's other releases.
const TagPrefix = "client-v"

// Source values reported to the web UI.
const (
	SourceRelease = "release"
	SourceHub     = "hub"
	SourceNone    = "none"
)

// Asset is one downloadable installer. URL is absolute for releases and a path on this hub for
// hub-served installers.
type Asset struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Label    string `json:"label"`
	Filename string `json:"filename"`
	URL      string `json:"url"`
	Size     int64  `json:"size,omitempty"`
}

// Info is what the web UI needs to offer a download.
type Info struct {
	Source     string  `json:"source"`
	Version    string  `json:"version,omitempty"`
	ReleaseURL string  `json:"release_url,omitempty"`
	Assets     []Asset `json:"assets"`
}

// Options configure a Resolver.
type Options struct {
	// Repo is "owner/repo" of the GitHub repository whose client releases are offered. Empty
	// disables the release source.
	Repo string
	// DistDir is the hub's OURCHIVE_DIST_DIR. Empty disables the hub source.
	DistDir string
	// HTTPClient and APIBase exist for tests; they default to a 10 second client and
	// https://api.github.com.
	HTTPClient *http.Client
	APIBase    string
	// TTL is how long a release lookup is reused. Default 10 minutes.
	TTL time.Duration
	// Now is the clock; tests replace it.
	Now func() time.Time
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// ValidRepo reports whether s looks like "owner/repo".
func ValidRepo(s string) bool {
	if !repoPattern.MatchString(s) {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

// Resolver finds the current installers. It is safe for concurrent use.
type Resolver struct {
	repo    string
	distDir string
	hc      *http.Client
	apiBase string
	ttl     time.Duration
	now     func() time.Time

	mu       sync.Mutex
	cached   Info
	cachedAt time.Time
	haveInfo bool
	failedAt time.Time
	failure  error
}

// failureBackoff is how long a failed lookup is remembered, so an unreachable GitHub doesn't turn
// every page view into a slow request.
const failureBackoff = time.Minute

// New builds a Resolver. An invalid Repo is ignored (the release source is simply off).
func New(o Options) *Resolver {
	r := &Resolver{
		distDir: o.DistDir,
		hc:      o.HTTPClient,
		apiBase: strings.TrimRight(o.APIBase, "/"),
		ttl:     o.TTL,
		now:     o.Now,
	}
	if ValidRepo(o.Repo) {
		r.repo = o.Repo
	}
	if r.hc == nil {
		r.hc = &http.Client{Timeout: 10 * time.Second}
	}
	if r.apiBase == "" {
		r.apiBase = "https://api.github.com"
	}
	if r.ttl <= 0 {
		r.ttl = 10 * time.Minute
	}
	if r.now == nil {
		r.now = time.Now
	}
	return r
}

// Resolve returns the installers to offer. It never fails: when nothing is available it reports
// Source "none" with an empty (non-nil) asset list.
func (r *Resolver) Resolve(ctx context.Context) Info {
	if r.repo != "" {
		if info, err := r.release(ctx); err == nil && len(info.Assets) > 0 {
			return info
		}
	}
	if info := r.hub(); len(info.Assets) > 0 {
		return info
	}
	return Info{Source: SourceNone, Assets: []Asset{}}
}

func (r *Resolver) hub() Info {
	info := Info{Source: SourceHub, Assets: []Asset{}}
	if r.distDir == "" {
		return info
	}
	for _, p := range Platforms {
		fi, err := os.Stat(filepath.Join(r.distDir, p.Filename))
		if err != nil || fi.IsDir() {
			continue
		}
		info.Assets = append(info.Assets, Asset{
			OS: p.OS, Arch: p.Arch, Label: p.Label, Filename: p.Filename,
			URL: "/downloads/" + p.Filename, Size: fi.Size(),
		})
	}
	return info
}

type ghRelease struct {
	TagName    string `json:"tag_name"`
	HTMLURL    string `json:"html_url"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (r *Resolver) release(ctx context.Context) (Info, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	if r.haveInfo && now.Sub(r.cachedAt) < r.ttl {
		return r.cached, nil
	}
	if r.failure != nil && now.Sub(r.failedAt) < failureBackoff {
		if r.haveInfo {
			return r.cached, nil // serve the stale answer while GitHub is unreachable
		}
		return Info{}, r.failure
	}

	info, err := r.fetchRelease(ctx)
	if err != nil {
		r.failure, r.failedAt = err, now
		if r.haveInfo {
			return r.cached, nil
		}
		return Info{}, err
	}
	r.failure = nil
	r.cached, r.cachedAt, r.haveInfo = info, now, true
	return info, nil
}

func (r *Resolver) fetchRelease(ctx context.Context) (Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.apiBase+"/repos/"+r.repo+"/releases?per_page=30", nil)
	if err != nil {
		return Info{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ourchive-hub")
	resp, err := r.hc.Do(req)
	if err != nil {
		return Info{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Info{}, fmt.Errorf("github releases: %s", resp.Status)
	}
	var releases []ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&releases); err != nil {
		return Info{}, fmt.Errorf("github releases: %w", err)
	}

	allowed := "https://github.com/" + r.repo + "/releases/download/"
	for _, rel := range releases { // newest first
		if rel.Draft || rel.Prerelease || !strings.HasPrefix(rel.TagName, TagPrefix) {
			continue
		}
		info := Info{
			Source:  SourceRelease,
			Version: strings.TrimPrefix(rel.TagName, "client-"),
			Assets:  []Asset{},
		}
		if strings.HasPrefix(rel.HTMLURL, "https://github.com/"+r.repo+"/") {
			info.ReleaseURL = rel.HTMLURL
		}
		for _, p := range Platforms {
			for _, a := range rel.Assets {
				if a.Name != p.Filename || len(a.URL) < len(allowed) || !strings.EqualFold(a.URL[:len(allowed)], allowed) {
					continue
				}
				info.Assets = append(info.Assets, Asset{
					OS: p.OS, Arch: p.Arch, Label: p.Label, Filename: p.Filename, URL: a.URL, Size: a.Size,
				})
				break
			}
		}
		return info, nil // the newest client release is the one to offer, even if it is incomplete
	}
	return Info{Source: SourceRelease, Assets: []Asset{}}, nil
}

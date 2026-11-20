package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Version and SigningPubKeyB64 are stamped at build time via -ldflags:
//
//	-X github.com/ourchive/ourchive/internal/updater.Version=v0.3.0
//	-X github.com/ourchive/ourchive/internal/updater.SigningPubKeyB64=<base64-ed25519-pub>
var (
	Version          = "0.3.0-dev"
	SigningPubKeyB64 = ""
)

type Asset struct {
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	Signature string `json:"signature,omitempty"` // Optional base64 Ed25519 signature over raw SHA256 hex bytes
}

type Manifest struct {
	Version     string           `json:"version"`
	PublishedAt string           `json:"published_at,omitempty"`
	Notes       string           `json:"notes,omitempty"`
	Assets      map[string]Asset `json:"assets"` // key: "<goos>/<goarch>", e.g. "windows/amd64"
}

type CheckResult struct {
	CurrentVersion string
	LatestVersion  string
	UpdateAvail    bool
	Asset          Asset
	Notes          string
}

func PlatformKey() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

// CleanupOldBinary removes leftover *.old executables created during a Windows self-update swap.
func CleanupOldBinary(exePath string) {
	if exePath == "" {
		return
	}
	_ = os.Remove(exePath + ".old")
}

// FetchManifest fetches a release manifest JSON either directly or from a GitHub Releases API URL.
func FetchManifest(ctx context.Context, manifestURL string) (*Manifest, error) {
	manifestURL = strings.TrimSpace(manifestURL)
	if manifestURL == "" {
		return nil, errors.New("update_url is not configured")
	}

	client := &http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ourchive-agent/"+Version)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update check returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return nil, err
	}

	// If pointing at GitHub's /releases/latest API, locate the attached manifest.json asset.
	if strings.Contains(manifestURL, "api.github.com/repos/") {
		var gh struct {
			Assets []struct {
				Name               string `json:"name"`
				BrowserDownloadURL string `json:"browser_download_url"`
			} `json:"assets"`
		}
		if json.Unmarshal(body, &gh) == nil && len(gh.Assets) > 0 {
			for _, a := range gh.Assets {
				if a.Name == "manifest.json" && a.BrowserDownloadURL != "" {
					return FetchManifest(ctx, a.BrowserDownloadURL)
				}
			}
		}
	}

	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse update manifest: %w", err)
	}
	if m.Version == "" {
		return nil, errors.New("update manifest missing version")
	}
	return &m, nil
}

// CheckForUpdate compares the local Version against the remote manifest for this OS/arch.
func CheckForUpdate(ctx context.Context, manifestURL, currentVersion string) (*CheckResult, error) {
	m, err := FetchManifest(ctx, manifestURL)
	if err != nil {
		return nil, err
	}
	asset, ok := m.Assets[PlatformKey()]
	if !ok || asset.URL == "" || asset.SHA256 == "" {
		return nil, fmt.Errorf("no release asset found for %s", PlatformKey())
	}
	cur := strings.TrimPrefix(strings.TrimSpace(currentVersion), "v")
	lat := strings.TrimPrefix(strings.TrimSpace(m.Version), "v")
	return &CheckResult{
		CurrentVersion: currentVersion,
		LatestVersion:  m.Version,
		UpdateAvail:    lat != "" && lat != cur,
		Asset:          asset,
		Notes:          m.Notes,
	}, nil
}

// VerifyChecksumAndSignature validates the downloaded file's SHA-256 and optional Ed25519 signature.
func VerifyChecksumAndSignature(filePath, expectedSHA256, sigB64, pubKeyB64 string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	actualHex := strings.ToLower(hex.EncodeToString(h.Sum(nil)))
	wantHex := strings.ToLower(strings.TrimSpace(expectedSHA256))
	if actualHex != wantHex {
		return fmt.Errorf("checksum mismatch: got %s, want %s", actualHex, wantHex)
	}

	if pubKeyB64 != "" {
		pubBytes, err := base64.StdEncoding.DecodeString(pubKeyB64)
		if err != nil || len(pubBytes) != ed25519.PublicKeySize {
			return errors.New("invalid embedded signing public key")
		}
		sigBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sigB64))
		if err != nil || len(sigBytes) != ed25519.SignatureSize {
			return errors.New("release missing valid Ed25519 signature")
		}
		if !ed25519.Verify(ed25519.PublicKey(pubBytes), []byte(actualHex), sigBytes) {
			return errors.New("Ed25519 release signature verification failed")
		}
	}
	return nil
}

// ApplyUpdate downloads the asset, verifies SHA-256 (+ Ed25519 if configured), and atomically swaps exePath.
func ApplyUpdate(ctx context.Context, asset Asset, exePath string) error {
	if exePath == "" {
		return errors.New("executable path is required")
	}
	dir := filepath.Dir(exePath)
	tmpPath := exePath + ".new"
	_ = os.Remove(tmpPath)

	client := &http.Client{Timeout: 5 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "ourchive-agent/"+Version)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download update: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download update returned HTTP %d", resp.StatusCode)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, io.LimitReader(resp.Body, 256*1024*1024))
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return closeErr
	}

	if err := VerifyChecksumAndSignature(tmpPath, asset.SHA256, asset.Signature, SigningPubKeyB64); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	// Atomic binary swap (works on Windows because running .exe can be renamed even while locked).
	oldPath := exePath + ".old"
	_ = os.Remove(oldPath)
	if err := os.Rename(exePath, oldPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("stage current binary: %w", err)
	}
	if err := os.Rename(tmpPath, exePath); err != nil {
		// Roll back if final rename fails.
		_ = os.Rename(oldPath, exePath)
		_ = os.Remove(tmpPath)
		return fmt.Errorf("activate new binary: %w", err)
	}
	return nil
}

package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyAndApplyUpdate(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "ourchive-agent.exe")
	if err := os.WriteFile(exePath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	newBytes := []byte("new-verified-binary-v0.4.0")
	sum := sha256.Sum256(newBytes)
	sumHex := hex.EncodeToString(sum[:])

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, []byte(sumHex))

	origPub := SigningPubKeyB64
	SigningPubKeyB64 = base64.StdEncoding.EncodeToString(pub)
	defer func() { SigningPubKeyB64 = origPub }()

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manifest.json" {
			_ = json.NewEncoder(w).Encode(Manifest{
				Version: "v0.4.0",
				Assets: map[string]Asset{
					PlatformKey(): {
						URL:       srv.URL + "/bin",
						SHA256:    sumHex,
						Signature: base64.StdEncoding.EncodeToString(sig),
					},
				},
			})
			return
		}
		_, _ = w.Write(newBytes)
	}))
	defer srv.Close()

	res, err := CheckForUpdate(context.Background(), srv.URL+"/manifest.json", "v0.3.0")
	if err != nil {
		t.Fatalf("CheckForUpdate: %v", err)
	}
	if !res.UpdateAvail || res.LatestVersion != "v0.4.0" {
		t.Fatalf("expected update v0.4.0, got %+v", res)
	}

	if err := ApplyUpdate(context.Background(), res.Asset, exePath); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}

	got, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newBytes) {
		t.Fatalf("expected updated binary %q, got %q", string(newBytes), string(got))
	}

	// Tampered signature must be rejected.
	badAsset := res.Asset
	badAsset.Signature = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	if err := ApplyUpdate(context.Background(), badAsset, exePath); err == nil {
		t.Fatal("expected tampered signature to be rejected")
	}
}

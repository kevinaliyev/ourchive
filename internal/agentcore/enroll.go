package agentcore

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
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

	"github.com/ourchive/ourchive/internal/compute"
	"github.com/ourchive/ourchive/pkg/protocol"
)

// EnrollOptions are the inputs to Enroll.
type EnrollOptions struct {
	HubURL string
	// Token is the one-time enrollment token from the hub's Contribution settings.
	Token string
	// Name is the machine's display name; empty means the hostname.
	Name string
	// ConfigPath overrides where the hub config is written; empty means DefaultHubConfigPath.
	ConfigPath string
	// ReadOnly hard-blocks remote writes at the agent layer regardless of hub settings.
	ReadOnly bool
	// AllowDirs optionally restricts which local directories can ever be mounted.
	AllowDirs []string
	// HTTPClient overrides the client used to call the hub (tests). Nil means a 30s-timeout client.
	HTTPClient *http.Client
}

// EnrollResult describes a completed enrollment.
type EnrollResult struct {
	ConfigPath  string
	HubURL      string
	NodeID      string
	DisplayName string
}

// ErrEnrollRejected wraps a non-201 response from the hub.
var ErrEnrollRejected = errors.New("enrollment rejected")

// Enroll generates an Ed25519 keypair, registers this machine with the hub using a one-time token
// and writes the per-hub config file. It does not start a connection; pass the returned
// ConfigPath to Runner.AddHub (or restart the agent) for that.
func Enroll(ctx context.Context, opts EnrollOptions) (*EnrollResult, error) {
	hubURL := NormalizeHubURL(opts.HubURL)
	if hubURL == "" {
		return nil, errors.New("hub URL is required")
	}
	if strings.TrimSpace(opts.Token) == "" {
		return nil, errors.New("enrollment token is required")
	}
	targetCfgPath := strings.TrimSpace(opts.ConfigPath)
	if targetCfgPath == "" {
		targetCfgPath = DefaultHubConfigPath(hubURL)
	}

	hostname, _ := os.Hostname()
	displayName := strings.TrimSpace(opts.Name)
	if displayName == "" {
		displayName = hostname
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate Ed25519 keypair: %w", err)
	}

	reqBody := protocol.EnrollRequest{
		Token:       strings.TrimSpace(opts.Token),
		DisplayName: displayName,
		PublicKey:   base64.StdEncoding.EncodeToString(pub),
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		Compute:     compute.ProbeCompute(),
	}
	payload, _ := json.Marshal(reqBody)

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hubURL+"/api/agent/enroll", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("enroll request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return nil, fmt.Errorf("%w (%d): %s", ErrEnrollRejected, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var enrollRes protocol.EnrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&enrollRes); err != nil {
		return nil, fmt.Errorf("decode enroll response: %w", err)
	}

	cleanedAllow := []string{}
	for _, d := range opts.AllowDirs {
		if abs, err := filepath.Abs(d); err == nil {
			cleanedAllow = append(cleanedAllow, filepath.Clean(abs))
		}
	}

	cfg := AgentConfig{
		HubURL:          hubURL,
		NodeID:          enrollRes.NodeID,
		DisplayName:     enrollRes.DisplayName,
		PublicKey:       base64.StdEncoding.EncodeToString(pub),
		PrivateKey:      base64.StdEncoding.EncodeToString(priv),
		AllowedBaseDirs: cleanedAllow,
		ReadOnly:        opts.ReadOnly,
	}
	if err := SaveConfig(targetCfgPath, &cfg); err != nil {
		return nil, fmt.Errorf("save agent config: %w", err)
	}

	return &EnrollResult{
		ConfigPath:  targetCfgPath,
		HubURL:      hubURL,
		NodeID:      cfg.NodeID,
		DisplayName: cfg.DisplayName,
	}, nil
}

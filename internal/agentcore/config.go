// Package agentcore is the contributor agent's engine: per-hub enrollment and configuration,
// the outbound WSS+yamux tunnel, and a Runner that keeps every enrolled hub connected while
// publishing its state. It never touches stdin, stdout or os.Exit, so the same engine runs
// inside the headless CLI (cmd/agent) and the desktop client.
package agentcore

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/ourchive/ourchive/pkg/protocol"
)

// AgentConfig is persisted locally per Hub under $HOME/.config/ourchive/hubs/<hub>.json.
type AgentConfig struct {
	HubURL            string                 `json:"hub_url"`
	NodeID            string                 `json:"node_id"`
	DisplayName       string                 `json:"display_name"`
	PublicKey         string                 `json:"public_key"`
	PrivateKey        string                 `json:"private_key"`
	AllowedBaseDirs   []string               `json:"allowed_base_dirs,omitempty"`
	ReadOnly          bool                   `json:"read_only,omitempty"`
	Paused            bool                   `json:"paused"`
	UploadCapMbps     int                    `json:"upload_cap_mbps"`
	AllowGroupCompute bool                   `json:"allow_group_compute"`
	Mounts            []protocol.FolderMount `json:"mounts,omitempty"`
}

// ConfigDir returns $HOME/.config/ourchive, the root shared by the agent and the desktop client.
func ConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".config", "ourchive")
	}
	return filepath.Join(home, ".config", "ourchive")
}

// HubsDir returns the directory holding one config file per enrolled hub.
func HubsDir() string {
	return filepath.Join(ConfigDir(), "hubs")
}

// HubSlug turns a hub URL into a stable file-name-safe identifier (its host, lowercased).
func HubSlug(hubURL string) string {
	u, err := url.Parse(strings.TrimSpace(hubURL))
	raw := hubURL
	if err == nil && u.Host != "" {
		raw = u.Host
	}
	var b strings.Builder
	for _, r := range strings.ToLower(raw) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-.")
	if s == "" {
		return "default"
	}
	return s
}

// DefaultHubConfigPath returns where the config for hubURL lives by default.
func DefaultHubConfigPath(hubURL string) string {
	return filepath.Join(HubsDir(), HubSlug(hubURL)+".json")
}

// DiscoverHubConfigs lists every *.json config in HubsDir.
func DiscoverHubConfigs() []string {
	dir := HubsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	paths := []string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	return paths
}

// SaveConfig writes cfg to path with owner-only permissions.
func SaveConfig(path string, cfg *AgentConfig) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// LoadConfig reads one hub config file.
func LoadConfig(path string) (*AgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg AgentConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// NormalizeHubURL trims whitespace and trailing slashes so the same hub always maps to one key.
func NormalizeHubURL(hubURL string) string {
	return strings.TrimRight(strings.TrimSpace(hubURL), "/")
}

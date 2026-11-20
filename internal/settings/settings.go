package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/ourchive/ourchive/internal/db"
)

const settingsKey = "hub_network_policy"

const (
	IdentityEmailOrUsername = "email_or_username"
	IdentityEmailOnly       = "email_only"
	IdentityUsernameOnly    = "username_only"
	IdentityBoth            = "both"
)

// EdgeStorageConfig configures an optional, provider-agnostic edge/object cache
// (S3-compatible like Cloudflare R2, Backblaze B2, MinIO, Wasabi, AWS, or Bunny.net Storage).
type EdgeStorageConfig struct {
	Enabled       bool   `json:"enabled"`
	Provider      string `json:"provider"` // "none" | "s3" | "bunny"
	Endpoint      string `json:"endpoint"` // S3 endpoint URL or Bunny storage host (e.g. storage.bunnycdn.com)
	Bucket        string `json:"bucket"`   // S3 bucket name or Bunny storage zone name
	Region        string `json:"region"`   // S3 region (e.g. "auto") or Bunny region prefix (e.g. "", "ny", "la", "sg")
	PublicBaseURL string `json:"public_base_url"`
	AccessKey     string `json:"access_key"`
	SecretKey     string `json:"secret_key,omitempty"`
	HasSecretKey  bool   `json:"has_secret_key"`
	MaxCacheMB    int    `json:"max_cache_mb"`
	TTLHours      int    `json:"ttl_hours"`
}

// HubSettings holds the live Host Admin policy for P2P, Hub relay fallback,
// opt-in edge storage, invite-only registration, and tiered audit logging.
type HubSettings struct {
	P2PEnabled         bool              `json:"p2p_enabled"`
	STUNServers        []string          `json:"stun_servers"`
	AllowRelayFallback bool              `json:"allow_relay_fallback"`
	AllowShareRelay    bool              `json:"allow_share_relay"`
	MaxRelayFileMB     int               `json:"max_relay_file_mb"`
	RelayCapMbps       int               `json:"relay_cap_mbps"`
	EdgeStorage        EdgeStorageConfig `json:"edge_storage"`

	// Registration & invite policy
	AuthIdentityMode       string `json:"auth_identity_mode"` // email_or_username | email_only | username_only | both
	AllowGroupAdminInvites bool   `json:"allow_group_admin_invites"`

	// Tiered audit logging policy (Host Admin rules supersede group settings)
	AuditCoreEnabled      bool `json:"audit_core_enabled"`
	AuditDownloadsEnabled bool `json:"audit_downloads_enabled"`
	AuditPreviewsEnabled  bool `json:"audit_previews_enabled"`
}

// PublicNetworkConfig is the non-secret subset exposed to logged-in users.
type PublicNetworkConfig struct {
	P2PEnabled             bool     `json:"p2p_enabled"`
	STUNServers            []string `json:"stun_servers"`
	AllowRelayFallback     bool     `json:"allow_relay_fallback"`
	AllowShareRelay        bool     `json:"allow_share_relay"`
	MaxRelayFileMB         int      `json:"max_relay_file_mb"`
	EdgeStorageActive      bool     `json:"edge_storage_active"`
	EdgeProvider           string   `json:"edge_provider"`
	AuthIdentityMode       string   `json:"auth_identity_mode"`
	AllowGroupAdminInvites bool     `json:"allow_group_admin_invites"`
	AuditCoreEnabled       bool     `json:"audit_core_enabled"`
	AuditDownloadsEnabled  bool     `json:"audit_downloads_enabled"`
	AuditPreviewsEnabled   bool     `json:"audit_previews_enabled"`
}

func DefaultSettings() HubSettings {
	return HubSettings{
		P2PEnabled:         true,
		STUNServers:        []string{"stun:stun.l.google.com:19302", "stun:stun.cloudflare.com:3478"},
		AllowRelayFallback: true,
		AllowShareRelay:    true,
		MaxRelayFileMB:     100,
		RelayCapMbps:       0,
		EdgeStorage: EdgeStorageConfig{
			Enabled:    false,
			Provider:   "none",
			MaxCacheMB: 100,
			TTLHours:   168,
		},
		AuthIdentityMode:       IdentityEmailOrUsername,
		AllowGroupAdminInvites: false,
		AuditCoreEnabled:       true,
		AuditDownloadsEnabled:  false,
		AuditPreviewsEnabled:   false,
	}
}

func NormalizeIdentityMode(mode string) string {
	switch mode {
	case IdentityEmailOnly, IdentityUsernameOnly, IdentityBoth, IdentityEmailOrUsername:
		return mode
	default:
		return IdentityEmailOrUsername
	}
}

type Store struct {
	db  *db.Store
	mu  sync.RWMutex
	cur HubSettings
}

func NewStore(ctx context.Context, database *db.Store) *Store {
	s := &Store{
		db:  database,
		cur: DefaultSettings(),
	}
	var raw string
	err := database.DB.QueryRowContext(ctx, `SELECT value_json FROM hub_settings WHERE key = ?`, settingsKey).Scan(&raw)
	if err == nil && raw != "" {
		// Start from defaults so newly added boolean fields (like AuditCoreEnabled) keep their defaults on upgrade.
		loaded := DefaultSettings()
		if json.Unmarshal([]byte(raw), &loaded) == nil {
			if len(loaded.STUNServers) == 0 {
				loaded.STUNServers = DefaultSettings().STUNServers
			}
			if loaded.EdgeStorage.Provider == "" {
				loaded.EdgeStorage.Provider = "none"
			}
			loaded.AuthIdentityMode = NormalizeIdentityMode(loaded.AuthIdentityMode)
			s.cur = loaded
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		// Keep defaults if table isn't populated yet.
	}
	return s
}

// Get returns the full settings including secrets (for internal Hub use).
func (s *Store) Get() HubSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := s.cur
	cp.EdgeStorage.HasSecretKey = cp.EdgeStorage.SecretKey != ""
	return cp
}

// GetRedacted returns settings safe to serialize to the Host Admin UI (secret key stripped).
func (s *Store) GetRedacted() HubSettings {
	cp := s.Get()
	cp.EdgeStorage.SecretKey = ""
	return cp
}

// Public returns the non-sensitive network & policy config for any authenticated user.
func (s *Store) Public() PublicNetworkConfig {
	cur := s.Get()
	return PublicNetworkConfig{
		P2PEnabled:             cur.P2PEnabled,
		STUNServers:            cur.STUNServers,
		AllowRelayFallback:     cur.AllowRelayFallback,
		AllowShareRelay:        cur.AllowShareRelay,
		MaxRelayFileMB:         cur.MaxRelayFileMB,
		EdgeStorageActive:      cur.EdgeStorage.Enabled && cur.EdgeStorage.Provider != "none",
		EdgeProvider:           cur.EdgeStorage.Provider,
		AuthIdentityMode:       cur.AuthIdentityMode,
		AllowGroupAdminInvites: cur.AllowGroupAdminInvites,
		AuditCoreEnabled:       cur.AuditCoreEnabled,
		AuditDownloadsEnabled:  cur.AuditDownloadsEnabled,
		AuditPreviewsEnabled:   cur.AuditPreviewsEnabled,
	}
}

// Save updates and persists HubSettings. If incoming SecretKey is blank, the existing secret is preserved.
func (s *Store) Save(ctx context.Context, next HubSettings) (HubSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if next.MaxRelayFileMB < 0 {
		next.MaxRelayFileMB = 0
	}
	if next.RelayCapMbps < 0 {
		next.RelayCapMbps = 0
	}
	if next.EdgeStorage.MaxCacheMB < 0 {
		next.EdgeStorage.MaxCacheMB = 0
	}
	if next.EdgeStorage.TTLHours < 0 {
		next.EdgeStorage.TTLHours = 0
	}
	if next.EdgeStorage.Provider != "s3" && next.EdgeStorage.Provider != "bunny" {
		next.EdgeStorage.Provider = "none"
	}
	if next.EdgeStorage.SecretKey == "" {
		next.EdgeStorage.SecretKey = s.cur.EdgeStorage.SecretKey
	}
	next.AuthIdentityMode = NormalizeIdentityMode(next.AuthIdentityMode)

	raw, err := json.Marshal(next)
	if err != nil {
		return HubSettings{}, err
	}
	_, err = s.db.DB.ExecContext(ctx, `
		INSERT INTO hub_settings (key, value_json, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at
	`, settingsKey, string(raw), time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return HubSettings{}, err
	}
	s.cur = next
	redacted := next
	redacted.EdgeStorage.HasSecretKey = next.EdgeStorage.SecretKey != ""
	redacted.EdgeStorage.SecretKey = ""
	return redacted, nil
}

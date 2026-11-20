package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ourchive/ourchive/internal/db"
	"github.com/ourchive/ourchive/internal/settings"
)

const (
	CategoryCore     = "core"
	CategoryDownload = "download"
	CategoryPreview  = "preview"
)

type Entry struct {
	ID         string         `json:"id"`
	CreatedAt  string         `json:"created_at"`
	Category   string         `json:"category"` // core | download | preview
	Action     string         `json:"action"`
	ActorID    string         `json:"actor_id,omitempty"`
	ActorLabel string         `json:"actor_label"`
	GroupID    string         `json:"group_id,omitempty"`
	TargetType string         `json:"target_type,omitempty"`
	TargetID   string         `json:"target_id,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	IPAddress  string         `json:"ip_address,omitempty"`
}

type Filter struct {
	Category string
	ActorID  string
	GroupID  string
	Search   string
	Limit    int
	Offset   int
}

type Service struct {
	store    *db.Store
	settings *settings.Store
}

func NewService(store *db.Store, settingsStore *settings.Store) *Service {
	return &Service{
		store:    store,
		settings: settingsStore,
	}
}

// ClientIP extracts the client IP from X-Forwarded-For, X-Real-IP, or RemoteAddr.
func ClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			return strings.TrimSpace(parts[0])
		}
	}
	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		return xri
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

// Log records an audit event if allowed by the global Host Admin policy.
func (s *Service) Log(ctx context.Context, e Entry) {
	if s == nil || s.store == nil {
		return
	}
	if e.Category == "" {
		e.Category = CategoryCore
	}
	cfg := s.settings.Get()
	switch e.Category {
	case CategoryCore:
		if !cfg.AuditCoreEnabled {
			return
		}
	case CategoryDownload:
		if !cfg.AuditDownloadsEnabled {
			return
		}
	case CategoryPreview:
		if !cfg.AuditPreviewsEnabled {
			return
		}
	default:
		if !cfg.AuditCoreEnabled {
			return
		}
	}

	if e.ID == "" {
		e.ID = uuid.NewString()
	}
	if e.CreatedAt == "" {
		e.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	metaJSON := "{}"
	if len(e.Metadata) > 0 {
		if b, err := json.Marshal(e.Metadata); err == nil {
			metaJSON = string(b)
		}
	}

	var actorID any
	if e.ActorID != "" {
		actorID = e.ActorID
	}
	var groupID any
	if e.GroupID != "" {
		groupID = e.GroupID
	}

	_, _ = s.store.DB.ExecContext(ctx, `
		INSERT INTO audit_logs (id, created_at, category, action, actor_id, actor_label, group_id, target_type, target_id, metadata_json, ip_address)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, e.ID, e.CreatedAt, e.Category, e.Action, actorID, e.ActorLabel, groupID, e.TargetType, e.TargetID, metaJSON, e.IPAddress)
}

// List returns audit log entries matching the given filter, newest first.
func (s *Service) List(ctx context.Context, f Filter) ([]Entry, int, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	if f.Offset < 0 {
		f.Offset = 0
	}

	where := []string{"1=1"}
	args := []any{}

	if f.Category != "" && f.Category != "all" {
		where = append(where, "category = ?")
		args = append(args, f.Category)
	}
	if f.ActorID != "" {
		where = append(where, "actor_id = ?")
		args = append(args, f.ActorID)
	}
	if f.GroupID != "" {
		where = append(where, "group_id = ?")
		args = append(args, f.GroupID)
	}
	if q := strings.TrimSpace(f.Search); q != "" {
		like := "%" + strings.ToLower(q) + "%"
		where = append(where, "(LOWER(action) LIKE ? OR LOWER(actor_label) LIKE ? OR LOWER(target_id) LIKE ? OR LOWER(metadata_json) LIKE ? OR LOWER(ip_address) LIKE ?)")
		args = append(args, like, like, like, like, like)
	}

	clause := strings.Join(where, " AND ")

	var total int
	if err := s.store.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM audit_logs WHERE `+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	queryArgs := append(append([]any{}, args...), f.Limit, f.Offset)
	rows, err := s.store.DB.QueryContext(ctx, `
		SELECT id, created_at, category, action, actor_id, actor_label, group_id, target_type, target_id, metadata_json, ip_address
		FROM audit_logs
		WHERE `+clause+`
		ORDER BY created_at DESC
		LIMIT ? OFFSET ?
	`, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	entries := []Entry{}
	for rows.Next() {
		var e Entry
		var actorID, groupID sql.NullString
		var metaRaw string
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.Category, &e.Action, &actorID, &e.ActorLabel, &groupID, &e.TargetType, &e.TargetID, &metaRaw, &e.IPAddress); err != nil {
			return nil, 0, err
		}
		if actorID.Valid {
			e.ActorID = actorID.String
		}
		if groupID.Valid {
			e.GroupID = groupID.String
		}
		if metaRaw != "" && metaRaw != "{}" {
			var m map[string]any
			if json.Unmarshal([]byte(metaRaw), &m) == nil {
				e.Metadata = m
			}
		}
		entries = append(entries, e)
	}
	return entries, total, rows.Err()
}

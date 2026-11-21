package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ourchive/ourchive/internal/audit"
	"github.com/ourchive/ourchive/internal/auth"
	"github.com/ourchive/ourchive/internal/shares"
)

func (h *hubApp) registerShareRoutes(appMux *http.ServeMux) {
	store := h.store
	authSvc := h.authSvc
	auditSvc := h.auditSvc
	eventBus := h.eventBus
	shareSvc := h.shareSvc

	// --- Phase 4: Share Links API ---
	appMux.HandleFunc("GET /api/shares", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFromContext(r.Context())
		list, err := shareSvc.List(r.Context(), u)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list share links")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"shares": list})
	}))

	appMux.HandleFunc("GET /api/shares/check-slug", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		slug := r.URL.Query().Get("slug")
		norm, ok, reason := shareSvc.IsSlugAvailable(r.Context(), slug)
		writeJSON(w, http.StatusOK, map[string]any{
			"slug":      norm,
			"available": ok,
			"reason":    reason,
		})
	}))

	appMux.HandleFunc("POST /api/shares", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFromContext(r.Context())
		var req shares.CreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		link, err := shareSvc.Create(r.Context(), req, u.ID)
		if errors.Is(err, shares.ErrSlugTaken) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		auditSvc.Log(r.Context(), audit.Entry{
			Category:   audit.CategoryCore,
			Action:     "share.created",
			ActorID:    u.ID,
			ActorLabel: u.Label(),
			TargetType: "share",
			TargetID:   link.Slug,
			Metadata:   map[string]any{"title": link.Title, "rel_path": link.RelPath},
			IPAddress:  audit.ClientIP(r),
		})
		writeJSON(w, http.StatusCreated, map[string]any{"share": link})
	}))

	appMux.HandleFunc("DELETE /api/shares/{slug}", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFromContext(r.Context())
		slug := r.PathValue("slug")
		ok, err := shareSvc.CanDelete(r.Context(), slug, u)
		if errors.Is(err, auth.ErrNotFound) {
			writeError(w, http.StatusNotFound, "share link not found")
			return
		}
		if err != nil || !ok {
			writeError(w, http.StatusForbidden, "only the link creator, machine owner, or Host Admin can revoke this link")
			return
		}
		if err := shareSvc.Delete(r.Context(), slug); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to revoke share link")
			return
		}
		auditSvc.Log(r.Context(), audit.Entry{
			Category:   audit.CategoryCore,
			Action:     "share.revoked",
			ActorID:    u.ID,
			ActorLabel: u.Label(),
			TargetType: "share",
			TargetID:   slug,
			IPAddress:  audit.ClientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	// --- Sticky Announcements & Admin Broadcasts ---
	appMux.HandleFunc("GET /api/announcements", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		rows, err := store.DB.QueryContext(r.Context(), `
			SELECT a.id, a.title, a.body, a.kind, a.dismissible, a.expires_at, a.created_by, COALESCE(u.display_name, ''), a.created_at
			FROM announcements a
			LEFT JOIN users u ON u.id = a.created_by
			ORDER BY a.created_at DESC
		`)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list announcements")
			return
		}
		defer rows.Close()
		type annView struct {
			ID          string `json:"id"`
			Title       string `json:"title"`
			Body        string `json:"body"`
			Kind        string `json:"kind"`
			Dismissible bool   `json:"dismissible"`
			ExpiresAt   string `json:"expires_at,omitempty"`
			CreatedBy   string `json:"created_by"`
			CreatorName string `json:"creator_name"`
			CreatedAt   string `json:"created_at"`
		}
		now := time.Now().UTC()
		includeExpired := r.URL.Query().Get("all") == "1" && auth.UserFromContext(r.Context()).Role == auth.RoleHostAdmin
		list := []annView{}
		for rows.Next() {
			var a annView
			var dism int
			var exp sql.NullString
			if err := rows.Scan(&a.ID, &a.Title, &a.Body, &a.Kind, &dism, &exp, &a.CreatedBy, &a.CreatorName, &a.CreatedAt); err != nil {
				continue
			}
			a.Dismissible = dism == 1
			if exp.Valid && exp.String != "" {
				a.ExpiresAt = exp.String
				if !includeExpired {
					if t, perr := time.Parse(time.RFC3339, exp.String); perr == nil && now.After(t) {
						continue
					}
				}
			}
			list = append(list, a)
		}
		writeJSON(w, http.StatusOK, map[string]any{"announcements": list})
	}))

	appMux.HandleFunc("POST /api/announcements", authSvc.RequireRoles([]string{auth.RoleHostAdmin}, func(w http.ResponseWriter, r *http.Request) {
		me := auth.UserFromContext(r.Context())
		var req struct {
			Title          string `json:"title"`
			Body           string `json:"body"`
			Kind           string `json:"kind"`
			Dismissible    *bool  `json:"dismissible"`
			ExpiresInHours int    `json:"expires_in_hours"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		req.Body = strings.TrimSpace(req.Body)
		req.Title = strings.TrimSpace(req.Title)
		if req.Body == "" {
			writeError(w, http.StatusBadRequest, "message body is required")
			return
		}
		switch req.Kind {
		case "broadcast", "notice", "warning", "error":
		default:
			req.Kind = "broadcast"
		}
		dismissible := true
		if req.Dismissible != nil {
			dismissible = *req.Dismissible
		}
		dismInt := 0
		if dismissible {
			dismInt = 1
		}
		now := time.Now().UTC()
		var expArg any
		var expStr string
		if req.ExpiresInHours > 0 {
			expStr = now.Add(time.Duration(req.ExpiresInHours) * time.Hour).Format(time.RFC3339)
			expArg = expStr
		}
		id := uuid.NewString()
		nowStr := now.Format(time.RFC3339)
		_, err := store.DB.ExecContext(r.Context(), `
			INSERT INTO announcements (id, title, body, kind, dismissible, expires_at, created_by, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, id, req.Title, req.Body, req.Kind, dismInt, expArg, me.ID, nowStr)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create announcement")
			return
		}
		auditSvc.Log(r.Context(), audit.Entry{
			Category:   audit.CategoryCore,
			Action:     "announcement.created",
			ActorID:    me.ID,
			ActorLabel: me.Label(),
			TargetType: "announcement",
			TargetID:   id,
			Metadata:   map[string]any{"kind": req.Kind, "title": req.Title},
			IPAddress:  audit.ClientIP(r),
		})
		eventBus.Publish("announcements.updated", map[string]any{"action": "created", "id": id})
		writeJSON(w, http.StatusCreated, map[string]any{
			"announcement": map[string]any{
				"id":           id,
				"title":        req.Title,
				"body":         req.Body,
				"kind":         req.Kind,
				"dismissible":  dismissible,
				"expires_at":   expStr,
				"created_by":   me.ID,
				"creator_name": me.DisplayName,
				"created_at":   nowStr,
			},
		})
	}))

	appMux.HandleFunc("DELETE /api/announcements/{id}", authSvc.RequireRoles([]string{auth.RoleHostAdmin}, func(w http.ResponseWriter, r *http.Request) {
		me := auth.UserFromContext(r.Context())
		id := r.PathValue("id")
		_, _ = store.DB.ExecContext(r.Context(), `DELETE FROM announcements WHERE id = ?`, id)
		auditSvc.Log(r.Context(), audit.Entry{
			Category:   audit.CategoryCore,
			Action:     "announcement.deleted",
			ActorID:    me.ID,
			ActorLabel: me.Label(),
			TargetType: "announcement",
			TargetID:   id,
			IPAddress:  audit.ClientIP(r),
		})
		eventBus.Publish("announcements.updated", map[string]any{"action": "deleted", "id": id})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

}

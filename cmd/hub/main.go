package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/ourchive/ourchive/internal/audit"
	"github.com/ourchive/ourchive/internal/auth"
	"github.com/ourchive/ourchive/internal/db"
	"github.com/ourchive/ourchive/internal/settings"
	"github.com/ourchive/ourchive/internal/shares"
	"github.com/ourchive/ourchive/internal/storage"
	"github.com/ourchive/ourchive/internal/tunnel"
	"github.com/ourchive/ourchive/internal/vfs"
	"github.com/ourchive/ourchive/pkg/protocol"
)

type healthResponse struct {
	Status    string `json:"status"`
	Version   string `json:"version"`
	ShareHost string `json:"share_host"`
	Time      string `json:"time"`
}

func main() {
	addr := envOr("OURCHIVE_ADDR", ":8080")
	shareHost := strings.ToLower(envOr("OURCHIVE_SHARE_HOST", "s.localhost"))
	dbPath := envOr("OURCHIVE_DB_PATH", "data/ourchive.db")
	thumbDir := envOr("OURCHIVE_THUMB_DIR", "data/thumbs")
	webDir := envOr("OURCHIVE_WEB_DIR", "web/dist")
	publicHubURL := envOr("OURCHIVE_PUBLIC_URL", "http://localhost"+addr)
	_ = os.MkdirAll(thumbDir, 0o755)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := db.OpenSQLite(ctx, dbPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	authSvc := auth.NewService(store)
	broker := tunnel.NewBroker(store)
	tagStore := vfs.NewTagStore(store)
	settingsStore := settings.NewStore(ctx, store)
	auditSvc := audit.NewService(store, settingsStore)
	authLimiter := newIPRateLimiter(12, 12) // 12 req/min with burst 12 per IP on auth/invite/enroll endpoints

	shareBaseURL := envOr("OURCHIVE_SHARE_URL", "http://"+shareHost+addr)
	shareSvc := shares.NewService(store, broker, settingsStore, shareBaseURL, thumbDir)

	appMux := http.NewServeMux()

	// --- Health ---
	appMux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, healthResponse{
			Status:    "ok",
			Version:   "0.3.0",
			ShareHost: shareHost,
			Time:      time.Now().UTC().Format(time.RFC3339),
		})
	})

	// --- Auth & First-Run Setup ---
	appMux.HandleFunc("GET /api/auth/status", func(w http.ResponseWriter, r *http.Request) {
		count, err := authSvc.UserCount(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"initialized":        count > 0,
			"auth_identity_mode": settingsStore.Get().AuthIdentityMode,
			"user":               authSvc.CurrentUser(r),
		})
	})

	appMux.HandleFunc("POST /api/auth/setup", func(w http.ResponseWriter, r *http.Request) {
		if !authLimiter.Allow(audit.ClientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "too many attempts; please wait a minute")
			return
		}
		var req struct {
			Email       string `json:"email"`
			Username    string `json:"username"`
			DisplayName string `json:"display_name"`
			Password    string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		u, err := authSvc.SetupHostAdmin(r.Context(), req.Email, req.Username, req.DisplayName, req.Password)
		if errors.Is(err, auth.ErrAlreadyInitialized) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		sid, exp, err := authSvc.CreateSession(r.Context(), u.ID, auth.DefaultSessionTTL)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create session")
			return
		}
		auth.SetSessionCookie(w, r, sid, exp)
		auditSvc.Log(r.Context(), audit.Entry{
			Category:   audit.CategoryCore,
			Action:     "auth.setup",
			ActorID:    u.ID,
			ActorLabel: u.Label(),
			TargetType: "user",
			TargetID:   u.ID,
			IPAddress:  audit.ClientIP(r),
		})
		writeJSON(w, http.StatusCreated, map[string]any{"user": u})
	})

	appMux.HandleFunc("POST /api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if !authLimiter.Allow(audit.ClientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "too many login attempts; please wait a minute")
			return
		}
		var req struct {
			Identifier string `json:"identifier"`
			Email      string `json:"email"`
			Username   string `json:"username"`
			Password   string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		ident := strings.TrimSpace(req.Identifier)
		if ident == "" {
			ident = strings.TrimSpace(req.Email)
		}
		if ident == "" {
			ident = strings.TrimSpace(req.Username)
		}
		u, err := authSvc.Authenticate(r.Context(), ident, req.Password)
		if errors.Is(err, auth.ErrInvalidCredentials) {
			auditSvc.Log(r.Context(), audit.Entry{
				Category:   audit.CategoryCore,
				Action:     "auth.login_failed",
				ActorLabel: ident,
				Metadata:   map[string]any{"identifier": ident},
				IPAddress:  audit.ClientIP(r),
			})
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "authentication error")
			return
		}
		sid, exp, err := authSvc.CreateSession(r.Context(), u.ID, auth.DefaultSessionTTL)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create session")
			return
		}
		auth.SetSessionCookie(w, r, sid, exp)
		auditSvc.Log(r.Context(), audit.Entry{
			Category:   audit.CategoryCore,
			Action:     "auth.login",
			ActorID:    u.ID,
			ActorLabel: u.Label(),
			IPAddress:  audit.ClientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"user": u})
	})

	appMux.HandleFunc("POST /api/auth/register", func(w http.ResponseWriter, r *http.Request) {
		if !authLimiter.Allow(audit.ClientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "too many registration attempts; please wait a minute")
			return
		}
		var req struct {
			InviteCode  string `json:"invite_code"`
			Email       string `json:"email"`
			Username    string `json:"username"`
			DisplayName string `json:"display_name"`
			Password    string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		mode := settingsStore.Get().AuthIdentityMode
		u, prefix, err := authSvc.RedeemInviteAndRegister(r.Context(), req.InviteCode, req.Email, req.Username, req.DisplayName, req.Password, mode)
		if errors.Is(err, auth.ErrInvalidInvite) {
			auditSvc.Log(r.Context(), audit.Entry{
				Category:   audit.CategoryCore,
				Action:     "auth.invite_rejected",
				ActorLabel: strings.TrimSpace(req.Username + " " + req.Email),
				IPAddress:  audit.ClientIP(r),
			})
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
		if errors.Is(err, auth.ErrUserExists) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		sid, exp, err := authSvc.CreateSession(r.Context(), u.ID, auth.DefaultSessionTTL)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create session")
			return
		}
		auth.SetSessionCookie(w, r, sid, exp)
		auditSvc.Log(r.Context(), audit.Entry{
			Category:   audit.CategoryCore,
			Action:     "auth.registered_via_invite",
			ActorID:    u.ID,
			ActorLabel: u.Label(),
			TargetType: "invite_key",
			TargetID:   u.InviteKeyID,
			Metadata: map[string]any{
				"invite_prefix": prefix,
				"invited_by":    u.InvitedBy,
				"role":          u.Role,
			},
			IPAddress: audit.ClientIP(r),
		})
		writeJSON(w, http.StatusCreated, map[string]any{"user": u})
	})

	appMux.HandleFunc("POST /api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		u := authSvc.CurrentUser(r)
		if c, err := r.Cookie(auth.SessionCookieName); err == nil && c.Value != "" {
			_ = authSvc.DeleteSession(r.Context(), c.Value)
		}
		auth.ClearSessionCookie(w, r)
		if u != nil {
			auditSvc.Log(r.Context(), audit.Entry{
				Category:   audit.CategoryCore,
				Action:     "auth.logout",
				ActorID:    u.ID,
				ActorLabel: u.Label(),
				IPAddress:  audit.ClientIP(r),
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	appMux.HandleFunc("POST /api/auth/logout-all", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFromContext(r.Context())
		_ = authSvc.DeleteAllSessionsForUser(r.Context(), u.ID)
		auth.ClearSessionCookie(w, r)
		auditSvc.Log(r.Context(), audit.Entry{
			Category:   audit.CategoryCore,
			Action:     "auth.logout_all",
			ActorID:    u.ID,
			ActorLabel: u.Label(),
			IPAddress:  audit.ClientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	appMux.HandleFunc("POST /api/auth/change-password", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFromContext(r.Context())
		var req struct {
			CurrentPassword string `json:"current_password"`
			NewPassword     string `json:"new_password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if err := authSvc.ChangePassword(r.Context(), u.ID, req.CurrentPassword, req.NewPassword); err != nil {
			if errors.Is(err, auth.ErrInvalidCredentials) {
				writeError(w, http.StatusUnauthorized, "current password is incorrect")
				return
			}
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Re-issue a fresh session for this browser after revoking all other sessions.
		sid, exp, err := authSvc.CreateSession(r.Context(), u.ID, auth.DefaultSessionTTL)
		if err == nil {
			auth.SetSessionCookie(w, r, sid, exp)
		}
		auditSvc.Log(r.Context(), audit.Entry{
			Category:   audit.CategoryCore,
			Action:     "auth.password_changed",
			ActorID:    u.ID,
			ActorLabel: u.Label(),
			IPAddress:  audit.ClientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	// --- Users & Groups (Domain Admin RBAC) ---
	appMux.HandleFunc("GET /api/users", authSvc.RequireRoles([]string{auth.RoleHostAdmin, auth.RoleGroupAdmin}, func(w http.ResponseWriter, r *http.Request) {
		users, err := authSvc.ListUsers(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list users")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"users": users})
	}))

	appMux.HandleFunc("POST /api/users", authSvc.RequireRoles([]string{auth.RoleHostAdmin}, func(w http.ResponseWriter, r *http.Request) {
		me := auth.UserFromContext(r.Context())
		var req struct {
			Email       string `json:"email"`
			Username    string `json:"username"`
			DisplayName string `json:"display_name"`
			Password    string `json:"password"`
			Role        string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		u, err := authSvc.CreateUserWithOptions(r.Context(), auth.CreateUserOptions{
			Email:        req.Email,
			Username:     req.Username,
			DisplayName:  req.DisplayName,
			Password:     req.Password,
			Role:         req.Role,
			IdentityMode: settingsStore.Get().AuthIdentityMode,
			InvitedBy:    me.ID,
		})
		if errors.Is(err, auth.ErrUserExists) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		auditSvc.Log(r.Context(), audit.Entry{
			Category:   audit.CategoryCore,
			Action:     "user.created",
			ActorID:    me.ID,
			ActorLabel: me.Label(),
			TargetType: "user",
			TargetID:   u.ID,
			Metadata:   map[string]any{"created_user": u.Label(), "role": u.Role},
			IPAddress:  audit.ClientIP(r),
		})
		writeJSON(w, http.StatusCreated, map[string]any{"user": u})
	}))

	appMux.HandleFunc("PATCH /api/users/{id}/invite-permissions", authSvc.RequireRoles([]string{auth.RoleHostAdmin}, func(w http.ResponseWriter, r *http.Request) {
		me := auth.UserFromContext(r.Context())
		id := r.PathValue("id")
		var req struct {
			CanInvite   bool `json:"can_invite"`
			InviteQuota int  `json:"invite_quota"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if err := authSvc.UpdateUserInvitePerms(r.Context(), id, req.CanInvite, req.InviteQuota); err != nil {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		auditSvc.Log(r.Context(), audit.Entry{
			Category:   audit.CategoryCore,
			Action:     "user.invite_perms_updated",
			ActorID:    me.ID,
			ActorLabel: me.Label(),
			TargetType: "user",
			TargetID:   id,
			Metadata:   map[string]any{"can_invite": req.CanInvite, "invite_quota": req.InviteQuota},
			IPAddress:  audit.ClientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	appMux.HandleFunc("DELETE /api/users/{id}", authSvc.RequireRoles([]string{auth.RoleHostAdmin}, func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		me := auth.UserFromContext(r.Context())
		if me != nil && me.ID == id {
			writeError(w, http.StatusBadRequest, "you cannot delete your own account")
			return
		}
		if err := authSvc.DeleteUser(r.Context(), id); err != nil {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		auditSvc.Log(r.Context(), audit.Entry{
			Category:   audit.CategoryCore,
			Action:     "user.deleted",
			ActorID:    me.ID,
			ActorLabel: me.Label(),
			TargetType: "user",
			TargetID:   id,
			IPAddress:  audit.ClientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	// --- Invite Keys (Invite-Only Registration & Lineage) ---
	appMux.HandleFunc("GET /api/invites", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		me := auth.UserFromContext(r.Context())
		cfg := settingsStore.Get()
		if !auth.CanUserCreateInvite(me, cfg.AllowGroupAdminInvites) && me.Role != auth.RoleHostAdmin {
			writeError(w, http.StatusForbidden, "you do not have invite permissions")
			return
		}
		keys, err := authSvc.ListInviteKeys(r.Context(), me)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list invite keys")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"invites": keys})
	}))

	appMux.HandleFunc("POST /api/invites", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		me := auth.UserFromContext(r.Context())
		cfg := settingsStore.Get()
		var req auth.CreateInviteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		key, err := authSvc.CreateInviteKey(r.Context(), me, req, cfg.AllowGroupAdminInvites)
		if errors.Is(err, auth.ErrInviteForbidden) || errors.Is(err, auth.ErrInviteQuotaReached) {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		auditSvc.Log(r.Context(), audit.Entry{
			Category:   audit.CategoryCore,
			Action:     "invite.created",
			ActorID:    me.ID,
			ActorLabel: me.Label(),
			TargetType: "invite_key",
			TargetID:   key.ID,
			Metadata: map[string]any{
				"prefix":   key.CodePrefix,
				"role":     key.Role,
				"max_uses": key.MaxUses,
				"group_id": key.GroupID,
				"note":     key.Note,
			},
			IPAddress: audit.ClientIP(r),
		})
		writeJSON(w, http.StatusCreated, map[string]any{"invite": key})
	}))

	appMux.HandleFunc("DELETE /api/invites/{id}", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		me := auth.UserFromContext(r.Context())
		id := r.PathValue("id")
		if err := authSvc.RevokeInviteKey(r.Context(), me, id); err != nil {
			writeError(w, http.StatusNotFound, "invite key not found or already revoked")
			return
		}
		auditSvc.Log(r.Context(), audit.Entry{
			Category:   audit.CategoryCore,
			Action:     "invite.revoked",
			ActorID:    me.ID,
			ActorLabel: me.Label(),
			TargetType: "invite_key",
			TargetID:   id,
			IPAddress:  audit.ClientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	appMux.HandleFunc("GET /api/groups", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		groups, err := authSvc.ListGroups(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list groups")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
	}))

	appMux.HandleFunc("POST /api/groups", authSvc.RequireRoles([]string{auth.RoleHostAdmin}, func(w http.ResponseWriter, r *http.Request) {
		me := auth.UserFromContext(r.Context())
		var req struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		g, err := authSvc.CreateGroup(r.Context(), req.Name, req.Description)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		auditSvc.Log(r.Context(), audit.Entry{
			Category:   audit.CategoryCore,
			Action:     "group.created",
			ActorID:    me.ID,
			ActorLabel: me.Label(),
			GroupID:    g.ID,
			TargetType: "group",
			TargetID:   g.ID,
			Metadata:   map[string]any{"name": g.Name},
			IPAddress:  audit.ClientIP(r),
		})
		writeJSON(w, http.StatusCreated, map[string]any{"group": g})
	}))

	appMux.HandleFunc("PATCH /api/groups/{id}/audit", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		me := auth.UserFromContext(r.Context())
		gid := r.PathValue("id")
		if !authSvc.IsGroupAdminOf(r.Context(), me, gid) {
			writeError(w, http.StatusForbidden, "only Host Admins or Group Admins of this group can edit audit settings")
			return
		}
		var req struct {
			AuditDownloads bool `json:"audit_downloads"`
			AuditPreviews  bool `json:"audit_previews"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		cfg := settingsStore.Get()
		if err := authSvc.UpdateGroupAuditSettings(r.Context(), gid, req.AuditDownloads, req.AuditPreviews, cfg.AuditDownloadsEnabled, cfg.AuditPreviewsEnabled); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		auditSvc.Log(r.Context(), audit.Entry{
			Category:   audit.CategoryCore,
			Action:     "group.audit_updated",
			ActorID:    me.ID,
			ActorLabel: me.Label(),
			GroupID:    gid,
			TargetType: "group",
			TargetID:   gid,
			Metadata:   map[string]any{"audit_downloads": req.AuditDownloads, "audit_previews": req.AuditPreviews},
			IPAddress:  audit.ClientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	appMux.HandleFunc("DELETE /api/groups/{id}", authSvc.RequireRoles([]string{auth.RoleHostAdmin}, func(w http.ResponseWriter, r *http.Request) {
		me := auth.UserFromContext(r.Context())
		gid := r.PathValue("id")
		if err := authSvc.DeleteGroup(r.Context(), gid); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to delete group")
			return
		}
		auditSvc.Log(r.Context(), audit.Entry{
			Category:   audit.CategoryCore,
			Action:     "group.deleted",
			ActorID:    me.ID,
			ActorLabel: me.Label(),
			TargetType: "group",
			TargetID:   gid,
			IPAddress:  audit.ClientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	// --- Audit Log Viewer (Host Admin: full; Group Admin: own group) ---
	appMux.HandleFunc("GET /api/audit-logs", authSvc.RequireRoles([]string{auth.RoleHostAdmin, auth.RoleGroupAdmin}, func(w http.ResponseWriter, r *http.Request) {
		me := auth.UserFromContext(r.Context())
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		offset, _ := strconv.Atoi(q.Get("offset"))
		filter := audit.Filter{
			Category: q.Get("category"),
			ActorID:  q.Get("actor_id"),
			GroupID:  q.Get("group_id"),
			Search:   q.Get("search"),
			Limit:    limit,
			Offset:   offset,
		}
		if me.Role != auth.RoleHostAdmin {
			if filter.GroupID == "" || !authSvc.IsGroupAdminOf(r.Context(), me, filter.GroupID) {
				writeError(w, http.StatusForbidden, "select a group you administer to view its audit log")
				return
			}
		}
		entries, total, err := auditSvc.List(r.Context(), filter)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load audit logs")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"entries": entries,
			"total":   total,
		})
	}))

	appMux.HandleFunc("POST /api/groups/{id}/members", authSvc.RequireRoles([]string{auth.RoleHostAdmin, auth.RoleGroupAdmin}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UserID string `json:"user_id"`
			Role   string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserID == "" {
			writeError(w, http.StatusBadRequest, "user_id is required")
			return
		}
		if err := authSvc.AddGroupMember(r.Context(), r.PathValue("id"), req.UserID, req.Role); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	appMux.HandleFunc("DELETE /api/groups/{id}/members/{userId}", authSvc.RequireRoles([]string{auth.RoleHostAdmin, auth.RoleGroupAdmin}, func(w http.ResponseWriter, r *http.Request) {
		if err := authSvc.RemoveGroupMember(r.Context(), r.PathValue("id"), r.PathValue("userId")); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	// --- Nodes & Contribution Settings ---
	appMux.HandleFunc("GET /api/nodes", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFromContext(r.Context())
		nodes, err := broker.ListNodesForUser(r.Context(), u)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list nodes")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
	}))

	appMux.HandleFunc("POST /api/nodes/enroll-token", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFromContext(r.Context())
		var req struct {
			DisplayName string `json:"display_name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		tok, exp, err := broker.CreateEnrollToken(r.Context(), u.ID, req.DisplayName)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create enrollment token")
			return
		}
		cmd := "go run ./cmd/agent enroll --hub " + publicHubURL + " --token " + tok
		if strings.TrimSpace(req.DisplayName) != "" {
			cmd += " --name \"" + strings.TrimSpace(req.DisplayName) + "\""
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"token":      tok,
			"expires_at": exp.Format(time.RFC3339),
			"command":    cmd,
		})
	}))

	appMux.HandleFunc("PATCH /api/nodes/{id}/config", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		nodeID := r.PathValue("id")
		u := auth.UserFromContext(r.Context())
		isOwner, err := broker.IsNodeOwner(r.Context(), nodeID, u)
		if errors.Is(err, auth.ErrNotFound) {
			writeError(w, http.StatusNotFound, "node not found")
			return
		}
		if err != nil || (!isOwner && u.Role != auth.RoleHostAdmin) {
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}

		var req struct {
			DisplayName       *string `json:"display_name"`
			Paused            *bool   `json:"paused"`
			UploadCapMbps     *int    `json:"upload_cap_mbps"`
			AllowGroupCompute *bool   `json:"allow_group_compute"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}

		// Non-owner Host Admins may ONLY pause/shut off a node — never mutate its owner settings.
		if !isOwner && (req.DisplayName != nil || req.UploadCapMbps != nil || req.AllowGroupCompute != nil) {
			writeError(w, http.StatusForbidden, "only the machine owner can modify node settings (Host Admins may only pause/shut off)")
			return
		}

		if isOwner && req.DisplayName != nil && strings.TrimSpace(*req.DisplayName) != "" {
			_, _ = store.DB.ExecContext(r.Context(), `UPDATE nodes SET display_name = ? WHERE id = ?`, strings.TrimSpace(*req.DisplayName), nodeID)
		}
		if req.Paused != nil {
			v := 0
			if *req.Paused {
				v = 1
			}
			_, _ = store.DB.ExecContext(r.Context(), `UPDATE nodes SET paused = ? WHERE id = ?`, v, nodeID)
		}
		if isOwner && req.UploadCapMbps != nil && *req.UploadCapMbps >= 0 {
			_, _ = store.DB.ExecContext(r.Context(), `UPDATE nodes SET upload_cap_mbps = ? WHERE id = ?`, *req.UploadCapMbps, nodeID)
		}
		if isOwner && req.AllowGroupCompute != nil {
			v := 0
			if *req.AllowGroupCompute {
				v = 1
			}
			_, _ = store.DB.ExecContext(r.Context(), `UPDATE nodes SET allow_group_compute = ? WHERE id = ?`, v, nodeID)
		}

		broker.PushConfig(r.Context(), nodeID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	appMux.HandleFunc("DELETE /api/nodes/{id}", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		nodeID := r.PathValue("id")
		u := auth.UserFromContext(r.Context())
		ok, err := broker.CanShutoffNode(r.Context(), nodeID, u)
		if errors.Is(err, auth.ErrNotFound) {
			writeError(w, http.StatusNotFound, "node not found")
			return
		}
		if err != nil || !ok {
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}
		broker.DisconnectNode(nodeID)
		_, _ = store.DB.ExecContext(r.Context(), `DELETE FROM nodes WHERE id = ?`, nodeID)
		auditSvc.Log(r.Context(), audit.Entry{
			Category:   audit.CategoryCore,
			Action:     "node.disconnected",
			ActorID:    u.ID,
			ActorLabel: u.Label(),
			TargetType: "node",
			TargetID:   nodeID,
			IPAddress:  audit.ClientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	appMux.HandleFunc("POST /api/nodes/{id}/mounts", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		nodeID := r.PathValue("id")
		u := auth.UserFromContext(r.Context())
		ok, err := broker.IsNodeOwner(r.Context(), nodeID, u)
		if errors.Is(err, auth.ErrNotFound) {
			writeError(w, http.StatusNotFound, "node not found")
			return
		}
		if err != nil || !ok {
			writeError(w, http.StatusForbidden, "only the machine owner can add shared paths")
			return
		}
		var req struct {
			Name      string `json:"name"`
			LocalPath string `json:"local_path"`
			Kind      string `json:"kind"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		req.LocalPath = strings.TrimSpace(req.LocalPath)
		if req.Name == "" || req.LocalPath == "" {
			writeError(w, http.StatusBadRequest, "name and local_path are required")
			return
		}
		kind := "folder"
		if req.Kind == "file" {
			kind = "file"
		}
		mount := protocol.FolderMount{
			ID:        uuid.NewString(),
			Name:      req.Name,
			LocalPath: req.LocalPath,
			Kind:      kind,
			Enabled:   true,
		}
		_, err = store.DB.ExecContext(r.Context(),
			`INSERT INTO mounts (id, node_id, name, local_path, kind, enabled, created_at) VALUES (?, ?, ?, ?, ?, 1, ?)`,
			mount.ID, nodeID, mount.Name, mount.LocalPath, mount.Kind, time.Now().UTC().Format(time.RFC3339),
		)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to save folder mount")
			return
		}
		broker.PushConfig(r.Context(), nodeID)
		writeJSON(w, http.StatusCreated, map[string]any{"mount": mount})
	}))

	appMux.HandleFunc("PATCH /api/nodes/{id}/mounts/{mountId}", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		nodeID := r.PathValue("id")
		mountID := r.PathValue("mountId")
		u := auth.UserFromContext(r.Context())
		ok, err := broker.IsNodeOwner(r.Context(), nodeID, u)
		if err != nil || !ok {
			writeError(w, http.StatusForbidden, "only the machine owner can modify shared paths")
			return
		}
		var req struct {
			Name      *string `json:"name"`
			Enabled   *bool   `json:"enabled"`
			AllowSync *bool   `json:"allow_sync"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
			_, _ = store.DB.ExecContext(r.Context(), `UPDATE mounts SET name = ? WHERE id = ? AND node_id = ?`, strings.TrimSpace(*req.Name), mountID, nodeID)
		}
		if req.Enabled != nil {
			v := 0
			if *req.Enabled {
				v = 1
			}
			_, _ = store.DB.ExecContext(r.Context(), `UPDATE mounts SET enabled = ? WHERE id = ? AND node_id = ?`, v, mountID, nodeID)
		}
		if req.AllowSync != nil {
			v := 0
			if *req.AllowSync {
				v = 1
			}
			_, _ = store.DB.ExecContext(r.Context(), `UPDATE mounts SET allow_sync = ? WHERE id = ? AND node_id = ?`, v, mountID, nodeID)
			if v == 0 {
				// Contributor revoked parity sync -> disable any existing subscriptions immediately.
				_, _ = store.DB.ExecContext(r.Context(), `UPDATE sync_subscriptions SET enabled = 0 WHERE source_mount_id = ?`, mountID)
			}
		}
		broker.PushConfig(r.Context(), nodeID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	appMux.HandleFunc("DELETE /api/nodes/{id}/mounts/{mountId}", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		nodeID := r.PathValue("id")
		mountID := r.PathValue("mountId")
		u := auth.UserFromContext(r.Context())
		ok, err := broker.IsNodeOwner(r.Context(), nodeID, u)
		if err != nil || !ok {
			writeError(w, http.StatusForbidden, "only the machine owner can remove shared paths")
			return
		}
		_, _ = store.DB.ExecContext(r.Context(), `DELETE FROM mounts WHERE id = ? AND node_id = ?`, mountID, nodeID)
		broker.PushConfig(r.Context(), nodeID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	appMux.HandleFunc("POST /api/nodes/{id}/pick-folder", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		nodeID := r.PathValue("id")
		u := auth.UserFromContext(r.Context())
		ok, err := broker.IsNodeOwner(r.Context(), nodeID, u)
		if err != nil || !ok {
			writeError(w, http.StatusForbidden, "only the machine owner can open local pickers")
			return
		}
		client, err := broker.HTTPClientForNode(nodeID)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "machine is offline")
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://agent/vfs/pick-folder", nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			writeError(w, http.StatusBadGateway, "folder picker failed: "+err.Error())
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))

	appMux.HandleFunc("POST /api/nodes/{id}/pick-file", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		nodeID := r.PathValue("id")
		u := auth.UserFromContext(r.Context())
		ok, err := broker.IsNodeOwner(r.Context(), nodeID, u)
		if err != nil || !ok {
			writeError(w, http.StatusForbidden, "only the machine owner can open local pickers")
			return
		}
		client, err := broker.HTTPClientForNode(nodeID)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "machine is offline")
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://agent/vfs/pick-file", nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			writeError(w, http.StatusBadGateway, "file picker failed: "+err.Error())
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))

	// --- Phase 2: Virtual Filesystem (Proxy over yamux) ---
	appMux.HandleFunc("GET /api/vfs/nodes/{nodeId}/mounts/{mountId}/list", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		nodeID := r.PathValue("nodeId")
		mountID := r.PathValue("mountId")
		client, err := broker.HTTPClientForNode(nodeID)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "machine is offline")
			return
		}
		q := url.Values{}
		q.Set("path", r.URL.Query().Get("path"))
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://agent/vfs/"+mountID+"/list?"+q.Encode(), nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			writeError(w, http.StatusBadGateway, "agent unreachable: "+err.Error())
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
			return
		}

		var listRes protocol.ListResponse
		if err := json.NewDecoder(resp.Body).Decode(&listRes); err != nil {
			writeError(w, http.StatusBadGateway, "invalid agent response")
			return
		}
		if tagMap, err := tagStore.TagsByMount(r.Context(), mountID); err == nil {
			for i := range listRes.Entries {
				if tags, ok := tagMap[listRes.Entries[i].Path]; ok {
					listRes.Entries[i].Tags = tags
				}
			}
		}
		writeJSON(w, http.StatusOK, listRes)
	}))

	vfsFileHandler := authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		pol := settingsStore.Get()
		if !pol.AllowRelayFallback {
			writeError(w, http.StatusForbidden, "Hub relay fallback is disabled by the Host Admin (use P2P direct transfer)")
			return
		}
		nodeID := r.PathValue("nodeId")
		mountID := r.PathValue("mountId")
		client, err := broker.HTTPClientForNode(nodeID)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "machine is offline")
			return
		}
		q := url.Values{}
		q.Set("path", r.URL.Query().Get("path"))
		if r.URL.Query().Get("download") == "1" {
			q.Set("download", "1")
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, "http://agent/vfs/"+mountID+"/file?"+q.Encode(), nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, hdr := range []string{"Range", "If-Range", "If-Modified-Since", "If-None-Match"} {
			if v := r.Header.Get(hdr); v != "" {
				req.Header.Set(hdr, v)
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			writeError(w, http.StatusBadGateway, "agent stream failed")
			return
		}
		defer resp.Body.Close()

		if pol.MaxRelayFileMB > 0 && resp.ContentLength > int64(pol.MaxRelayFileMB)*1024*1024 && r.Header.Get("Range") == "" {
			writeError(w, http.StatusRequestEntityTooLarge, "file exceeds Host Admin Hub relay size limit")
			return
		}

		for _, hdr := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Content-Disposition", "Last-Modified", "ETag"} {
			if v := resp.Header.Get(hdr); v != "" {
				w.Header().Set(hdr, v)
			}
		}
		// Neutralize stored XSS in uploaded HTML/XHTML/SVG files served via Hub.
		ctLower := strings.ToLower(resp.Header.Get("Content-Type"))
		if strings.Contains(ctLower, "text/html") || strings.Contains(ctLower, "application/xhtml") || strings.Contains(ctLower, "image/svg") {
			w.Header().Set("Content-Disposition", "attachment")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		w.WriteHeader(resp.StatusCode)
		if r.Method == http.MethodGet && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			rangeHdr := r.Header.Get("Range")
			if rangeHdr == "" || strings.HasPrefix(rangeHdr, "bytes=0-") {
				u := auth.UserFromContext(r.Context())
				cat := audit.CategoryPreview
				act := "file.previewed"
				if r.URL.Query().Get("download") == "1" {
					cat = audit.CategoryDownload
					act = "file.downloaded"
				}
				auditSvc.Log(r.Context(), audit.Entry{
					Category:   cat,
					Action:     act,
					ActorID:    u.ID,
					ActorLabel: u.Label(),
					TargetType: "file",
					TargetID:   r.URL.Query().Get("path"),
					Metadata:   map[string]any{"node_id": nodeID, "mount_id": mountID},
					IPAddress:  audit.ClientIP(r),
				})
			}
		}
		if r.Method != http.MethodHead {
			if pol.RelayCapMbps > 0 {
				copyThrottled(w, resp.Body, int64(pol.RelayCapMbps)*125_000)
			} else {
				_, _ = io.Copy(w, resp.Body)
			}
		}
	})
	appMux.HandleFunc("GET /api/vfs/nodes/{nodeId}/mounts/{mountId}/file", vfsFileHandler)
	appMux.HandleFunc("HEAD /api/vfs/nodes/{nodeId}/mounts/{mountId}/file", vfsFileHandler)

	// --- Phase 3: Hub-Cached Tiny WebP Thumbnails (serves even when node is offline) ---
	appMux.HandleFunc("GET /api/vfs/nodes/{nodeId}/mounts/{mountId}/thumb", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		nodeID := r.PathValue("nodeId")
		mountID := r.PathValue("mountId")
		relPath := r.URL.Query().Get("path")

		sum := sha256.Sum256([]byte(nodeID + "|" + mountID + "|" + relPath))
		cacheFile := filepath.Join(thumbDir, hex.EncodeToString(sum[:16])+".webp")

		if data, err := os.ReadFile(cacheFile); err == nil && len(data) > 0 {
			w.Header().Set("Content-Type", "image/webp")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}

		client, err := broker.HTTPClientForNode(nodeID)
		if err != nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		q := url.Values{}
		q.Set("path", relPath)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://agent/vfs/"+mountID+"/thumb?"+q.Encode(), nil)
		if err != nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		ct := resp.Header.Get("Content-Type")
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
		if err != nil || len(body) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Cache tiny WebP thumbnails on Hub disk for instant loads & offline fallback.
		if strings.Contains(ct, "webp") && len(body) <= 512*1024 {
			tmp := cacheFile + ".tmp"
			if os.WriteFile(tmp, body, 0o644) == nil {
				_ = os.Rename(tmp, cacheFile)
			}
		}

		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))

	appMux.HandleFunc("PUT /api/vfs/nodes/{nodeId}/mounts/{mountId}/file", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		nodeID := r.PathValue("nodeId")
		mountID := r.PathValue("mountId")
		u := auth.UserFromContext(r.Context())
		ok, err := broker.IsNodeOwner(r.Context(), nodeID, u)
		if err != nil || !ok {
			writeError(w, http.StatusForbidden, "only the machine owner can write files to this node")
			return
		}
		client, err := broker.HTTPClientForNode(nodeID)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "machine is offline")
			return
		}
		q := url.Values{}
		q.Set("path", r.URL.Query().Get("path"))
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPut, "http://agent/vfs/"+mountID+"/file?"+q.Encode(), r.Body)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		req.ContentLength = r.ContentLength
		resp, err := client.Do(req)
		if err != nil {
			writeError(w, http.StatusBadGateway, "upload stream failed: "+err.Error())
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			auditSvc.Log(r.Context(), audit.Entry{
				Category:   audit.CategoryCore,
				Action:     "file.uploaded",
				ActorID:    u.ID,
				ActorLabel: u.Label(),
				TargetType: "file",
				TargetID:   r.URL.Query().Get("path"),
				Metadata:   map[string]any{"node_id": nodeID, "mount_id": mountID},
				IPAddress:  audit.ClientIP(r),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))

	appMux.HandleFunc("POST /api/vfs/nodes/{nodeId}/mounts/{mountId}/mkdir", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		nodeID := r.PathValue("nodeId")
		mountID := r.PathValue("mountId")
		u := auth.UserFromContext(r.Context())
		ok, err := broker.IsNodeOwner(r.Context(), nodeID, u)
		if err != nil || !ok {
			writeError(w, http.StatusForbidden, "only the machine owner can create folders on this node")
			return
		}
		client, err := broker.HTTPClientForNode(nodeID)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "machine is offline")
			return
		}
		q := url.Values{}
		q.Set("path", r.URL.Query().Get("path"))
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://agent/vfs/"+mountID+"/mkdir?"+q.Encode(), nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			writeError(w, http.StatusBadGateway, "mkdir failed")
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			auditSvc.Log(r.Context(), audit.Entry{
				Category:   audit.CategoryCore,
				Action:     "folder.created",
				ActorID:    u.ID,
				ActorLabel: u.Label(),
				TargetType: "folder",
				TargetID:   r.URL.Query().Get("path"),
				Metadata:   map[string]any{"node_id": nodeID, "mount_id": mountID},
				IPAddress:  audit.ClientIP(r),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))

	appMux.HandleFunc("DELETE /api/vfs/nodes/{nodeId}/mounts/{mountId}/entry", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		nodeID := r.PathValue("nodeId")
		mountID := r.PathValue("mountId")
		u := auth.UserFromContext(r.Context())
		ok, err := broker.IsNodeOwner(r.Context(), nodeID, u)
		if err != nil || !ok {
			writeError(w, http.StatusForbidden, "only the machine owner can delete files on this node")
			return
		}
		client, err := broker.HTTPClientForNode(nodeID)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "machine is offline")
			return
		}
		relPath := r.URL.Query().Get("path")
		q := url.Values{}
		q.Set("path", relPath)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodDelete, "http://agent/vfs/"+mountID+"/entry?"+q.Encode(), nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			writeError(w, http.StatusBadGateway, "delete failed")
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			tagStore.CleanupDeletedPath(r.Context(), mountID, relPath)
			auditSvc.Log(r.Context(), audit.Entry{
				Category:   audit.CategoryCore,
				Action:     "file.deleted",
				ActorID:    u.ID,
				ActorLabel: u.Label(),
				TargetType: "file",
				TargetID:   relPath,
				Metadata:   map[string]any{"node_id": nodeID, "mount_id": mountID},
				IPAddress:  audit.ClientIP(r),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))

	appMux.HandleFunc("POST /api/vfs/nodes/{nodeId}/mounts/{mountId}/reveal", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		nodeID := r.PathValue("nodeId")
		mountID := r.PathValue("mountId")
		u := auth.UserFromContext(r.Context())
		ok, err := broker.IsNodeOwner(r.Context(), nodeID, u)
		if err != nil || !ok {
			writeError(w, http.StatusForbidden, "only the machine owner can reveal files locally")
			return
		}
		client, err := broker.HTTPClientForNode(nodeID)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "machine is offline")
			return
		}
		q := url.Values{}
		q.Set("path", r.URL.Query().Get("path"))
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://agent/vfs/"+mountID+"/reveal?"+q.Encode(), nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			writeError(w, http.StatusBadGateway, "reveal failed")
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))

	// --- Opt-In Parity Sync Subscriptions (Dual Consent: Contributor allows + User subscribes) ---
	appMux.HandleFunc("GET /api/sync/subscriptions", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFromContext(r.Context())
		rows, err := store.DB.QueryContext(r.Context(), `
			SELECT s.id, s.source_node_id, sn.display_name, s.source_mount_id, m.name, m.allow_sync,
			       s.target_node_id, tn.display_name, s.target_path, s.enabled, s.created_at
			FROM sync_subscriptions s
			JOIN nodes sn ON sn.id = s.source_node_id
			JOIN mounts m ON m.id = s.source_mount_id
			JOIN nodes tn ON tn.id = s.target_node_id
			WHERE s.user_id = ?
			ORDER BY s.created_at DESC
		`, u.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list sync subscriptions")
			return
		}
		defer rows.Close()
		type subView struct {
			ID              string `json:"id"`
			SourceNodeID    string `json:"source_node_id"`
			SourceNodeName  string `json:"source_node_name"`
			SourceMountID   string `json:"source_mount_id"`
			SourceMountName string `json:"source_mount_name"`
			ContributorOK   bool   `json:"contributor_ok"`
			TargetNodeID    string `json:"target_node_id"`
			TargetNodeName  string `json:"target_node_name"`
			TargetPath      string `json:"target_path"`
			Enabled         bool   `json:"enabled"`
			CreatedAt       string `json:"created_at"`
		}
		subs := []subView{}
		for rows.Next() {
			var sv subView
			var allowInt, enInt int
			if err := rows.Scan(&sv.ID, &sv.SourceNodeID, &sv.SourceNodeName, &sv.SourceMountID, &sv.SourceMountName, &allowInt, &sv.TargetNodeID, &sv.TargetNodeName, &sv.TargetPath, &enInt, &sv.CreatedAt); err != nil {
				continue
			}
			sv.ContributorOK = allowInt == 1
			sv.Enabled = enInt == 1 && sv.ContributorOK
			subs = append(subs, sv)
		}
		writeJSON(w, http.StatusOK, map[string]any{"subscriptions": subs})
	}))

	appMux.HandleFunc("POST /api/sync/subscriptions", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFromContext(r.Context())
		var req struct {
			SourceNodeID  string `json:"source_node_id"`
			SourceMountID string `json:"source_mount_id"`
			TargetNodeID  string `json:"target_node_id"`
			TargetPath    string `json:"target_path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SourceMountID == "" || req.TargetNodeID == "" {
			writeError(w, http.StatusBadRequest, "source_mount_id and target_node_id are required")
			return
		}
		var allowSync int
		if err := store.DB.QueryRowContext(r.Context(), `SELECT allow_sync FROM mounts WHERE id = ?`, req.SourceMountID).Scan(&allowSync); err != nil || allowSync != 1 {
			writeError(w, http.StatusForbidden, "contributor has not opted this folder into parity sync")
			return
		}
		ok, err := broker.IsNodeOwner(r.Context(), req.TargetNodeID, u)
		if err != nil || !ok {
			writeError(w, http.StatusForbidden, "target machine must belong to you")
			return
		}
		id := uuid.NewString()
		_, err = store.DB.ExecContext(r.Context(), `
			INSERT INTO sync_subscriptions (id, user_id, source_node_id, source_mount_id, target_node_id, target_path, enabled, created_at)
			VALUES (?, ?, ?, ?, ?, ?, 1, ?)
			ON CONFLICT(user_id, source_mount_id, target_node_id) DO UPDATE SET target_path = excluded.target_path, enabled = 1
		`, id, u.ID, req.SourceNodeID, req.SourceMountID, req.TargetNodeID, strings.TrimSpace(req.TargetPath), time.Now().UTC().Format(time.RFC3339))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id})
	}))

	appMux.HandleFunc("DELETE /api/sync/subscriptions/{id}", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFromContext(r.Context())
		_, _ = store.DB.ExecContext(r.Context(), `DELETE FROM sync_subscriptions WHERE id = ? AND user_id = ?`, r.PathValue("id"), u.ID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	// --- Phase 2: Tags & Tag-Composed Shared Trees ---
	appMux.HandleFunc("GET /api/tags", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		tags, err := tagStore.ListTags(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list tags")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"tags": tags})
	}))

	appMux.HandleFunc("POST /api/tags", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFromContext(r.Context())
		var req struct {
			Slug        string `json:"slug"`
			Description string `json:"description"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		tag, err := tagStore.CreateOrGetTag(r.Context(), req.Slug, req.Description, u.ID)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"tag": tag})
	}))

	appMux.HandleFunc("DELETE /api/tags/{id}", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFromContext(r.Context())
		tagID := r.PathValue("id")
		ok, err := tagStore.CanDeleteTag(r.Context(), tagID, u.ID, u.Role)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "tag not found")
			return
		}
		if err != nil || !ok {
			writeError(w, http.StatusForbidden, "only the tag creator or Host Admin can delete this tag")
			return
		}
		if err := tagStore.DeleteTag(r.Context(), tagID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to delete tag")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	appMux.HandleFunc("POST /api/tags/attach", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFromContext(r.Context())
		var req struct {
			TagSlug string `json:"tag_slug"`
			NodeID  string `json:"node_id"`
			MountID string `json:"mount_id"`
			RelPath string `json:"rel_path"`
			IsDir   bool   `json:"is_dir"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		tag, err := tagStore.AttachTag(r.Context(), req.TagSlug, req.NodeID, req.MountID, req.RelPath, req.IsDir, u.ID)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"tag": tag})
	}))

	appMux.HandleFunc("DELETE /api/tags/detach", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TagSlug string `json:"tag_slug"`
			MountID string `json:"mount_id"`
			RelPath string `json:"rel_path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if err := tagStore.DetachTag(r.Context(), req.TagSlug, req.MountID, req.RelPath); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	appMux.HandleFunc("GET /api/trees/{slug}", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFromContext(r.Context())
		items, err := tagStore.ListTreeItems(r.Context(), r.PathValue("slug"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load shared tree")
			return
		}
		nodes, _ := broker.ListNodesForUser(r.Context(), u)
		nodeByID := map[string]tunnel.NodeView{}
		for _, n := range nodes {
			nodeByID[n.ID] = n
		}
		for i := range items {
			if nv, ok := nodeByID[items[i].NodeID]; ok {
				items[i].NodeStatus = nv.Status
				items[i].Traffic = nv.Traffic
			} else {
				items[i].NodeStatus = protocol.NodeStatusOffline
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"slug":  vfs.NormalizeTagSlug(r.PathValue("slug")),
			"items": items,
		})
	}))

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
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	// --- Settings (Public Network Config + Host Admin Policy & Opt-In Edge Storage) ---
	appMux.HandleFunc("GET /api/settings/public", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, settingsStore.Public())
	}))

	appMux.HandleFunc("GET /api/settings", authSvc.RequireRoles([]string{auth.RoleHostAdmin}, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, settingsStore.GetRedacted())
	}))

	appMux.HandleFunc("PATCH /api/settings", authSvc.RequireRoles([]string{auth.RoleHostAdmin}, func(w http.ResponseWriter, r *http.Request) {
		me := auth.UserFromContext(r.Context())
		var req settings.HubSettings
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		saved, err := settingsStore.Save(r.Context(), req)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to save settings")
			return
		}
		// Host Admin rules supersede group audit toggles: turn off group flags when disabled globally.
		if !saved.AuditDownloadsEnabled {
			_, _ = store.DB.ExecContext(r.Context(), `UPDATE groups SET audit_downloads = 0`)
		}
		if !saved.AuditPreviewsEnabled {
			_, _ = store.DB.ExecContext(r.Context(), `UPDATE groups SET audit_previews = 0`)
		}
		auditSvc.Log(r.Context(), audit.Entry{
			Category:   audit.CategoryCore,
			Action:     "settings.updated",
			ActorID:    me.ID,
			ActorLabel: me.Label(),
			Metadata: map[string]any{
				"auth_identity_mode":        saved.AuthIdentityMode,
				"allow_group_admin_invites": saved.AllowGroupAdminInvites,
				"audit_core_enabled":        saved.AuditCoreEnabled,
				"audit_downloads_enabled":   saved.AuditDownloadsEnabled,
				"audit_previews_enabled":    saved.AuditPreviewsEnabled,
			},
			IPAddress: audit.ClientIP(r),
		})
		writeJSON(w, http.StatusOK, saved)
	}))

	appMux.HandleFunc("POST /api/settings/test-edge", authSvc.RequireRoles([]string{auth.RoleHostAdmin}, func(w http.ResponseWriter, r *http.Request) {
		cfg := settingsStore.Get().EdgeStorage
		prov, err := storage.NewEdgeProvider(cfg)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := prov.TestConnection(r.Context()); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	// --- WebRTC P2P Signaling (Hub exchanges ~2KB SDP only; file bytes go PC <-> Browser) ---
	appMux.HandleFunc("POST /api/p2p/nodes/{nodeId}/offer", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		pol := settingsStore.Get()
		if !pol.P2PEnabled {
			writeError(w, http.StatusForbidden, "WebRTC P2P is disabled by Host Admin")
			return
		}
		var req struct {
			SDP string `json:"sdp"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SDP == "" {
			writeError(w, http.StatusBadRequest, "sdp is required")
			return
		}
		client, err := broker.HTTPClientForNode(r.PathValue("nodeId"))
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "machine is offline")
			return
		}
		payload, _ := json.Marshal(map[string]any{
			"sdp":          req.SDP,
			"stun_servers": pol.STUNServers,
		})
		agentReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://agent/p2p/offer", strings.NewReader(string(payload)))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		agentReq.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(agentReq)
		if err != nil {
			writeError(w, http.StatusBadGateway, "p2p signaling failed: "+err.Error())
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))

	// --- Phase 5: 3-Tier Converter (Agent Native GPU Compute Proxy & Job History) ---
	appMux.HandleFunc("GET /api/compute/nodes", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFromContext(r.Context())
		all, err := broker.ListNodesForUser(r.Context(), u)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list compute nodes")
			return
		}
		eligible := []tunnel.NodeView{}
		for _, n := range all {
			if n.Status != protocol.NodeStatusOnline || !n.Compute.Enabled {
				continue
			}
			if n.IsOwner || n.AllowGroupCompute {
				eligible = append(eligible, n)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"nodes": eligible})
	}))

	appMux.HandleFunc("POST /api/compute/convert", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFromContext(r.Context())
		nodeID := r.URL.Query().Get("node_id")
		format := r.URL.Query().Get("format")
		preset := r.URL.Query().Get("preset")
		filename := r.URL.Query().Get("filename")
		if nodeID == "" || format == "" {
			writeError(w, http.StatusBadRequest, "node_id and format are required")
			return
		}

		// Verify user is allowed to use this node's compute (owner, host_admin, or allow_group_compute=1).
		nodes, _ := broker.ListNodesForUser(r.Context(), u)
		allowed := false
		for _, n := range nodes {
			if n.ID == nodeID && n.Status == protocol.NodeStatusOnline && (n.IsOwner || n.AllowGroupCompute) {
				allowed = true
				break
			}
		}
		if !allowed {
			writeError(w, http.StatusForbidden, "this machine is offline or does not allow group GPU jobs")
			return
		}

		client, err := broker.HTTPClientForNode(nodeID)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "compute machine is offline")
			return
		}

		jobID := uuid.NewString()
		nowStr := time.Now().UTC().Format(time.RFC3339)
		_, _ = store.DB.ExecContext(r.Context(), `
			INSERT INTO compute_jobs (id, target_node_id, requested_by, output_format, preset, status, progress_pct, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 'running', 10, ?, ?)
		`, jobID, nodeID, u.ID, format, preset, nowStr, nowStr)

		q := url.Values{}
		q.Set("format", format)
		q.Set("preset", preset)
		q.Set("filename", filename)
		agentReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://agent/compute/convert?"+q.Encode(), r.Body)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		agentReq.ContentLength = r.ContentLength

		resp, err := client.Do(agentReq)
		if err != nil {
			_, _ = store.DB.ExecContext(r.Context(), `UPDATE compute_jobs SET status = 'failed', error_message = ?, updated_at = ? WHERE id = ?`, err.Error(), time.Now().UTC().Format(time.RFC3339), jobID)
			writeError(w, http.StatusBadGateway, "compute agent failed: "+err.Error())
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			errBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			_, _ = store.DB.ExecContext(r.Context(), `UPDATE compute_jobs SET status = 'failed', error_message = ?, updated_at = ? WHERE id = ?`, string(errBytes), time.Now().UTC().Format(time.RFC3339), jobID)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(errBytes)
			return
		}

		_, _ = store.DB.ExecContext(r.Context(), `UPDATE compute_jobs SET status = 'completed', progress_pct = 100, updated_at = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339), jobID)
		for _, hdr := range []string{"Content-Type", "Content-Length", "Content-Disposition", "X-Ourchive-Encoder", "X-Ourchive-Duration-Ms"} {
			if v := resp.Header.Get(hdr); v != "" {
				w.Header().Set(hdr, v)
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, resp.Body)
	}))

	appMux.HandleFunc("GET /api/compute/jobs", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFromContext(r.Context())
		rows, err := store.DB.QueryContext(r.Context(), `
			SELECT cj.id, cj.target_node_id, n.display_name, cj.output_format, cj.preset, cj.status, cj.error_message, cj.created_at
			FROM compute_jobs cj
			JOIN nodes n ON n.id = cj.target_node_id
			WHERE cj.requested_by = ?
			ORDER BY cj.created_at DESC
			LIMIT 20
		`, u.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load compute jobs")
			return
		}
		defer rows.Close()
		type jobView struct {
			ID           string `json:"id"`
			TargetNodeID string `json:"target_node_id"`
			NodeName     string `json:"node_name"`
			OutputFormat string `json:"output_format"`
			Preset       string `json:"preset"`
			Status       string `json:"status"`
			ErrorMessage string `json:"error_message,omitempty"`
			CreatedAt    string `json:"created_at"`
		}
		jobs := []jobView{}
		for rows.Next() {
			var j jobView
			if rows.Scan(&j.ID, &j.TargetNodeID, &j.NodeName, &j.OutputFormat, &j.Preset, &j.Status, &j.ErrorMessage, &j.CreatedAt) == nil {
				jobs = append(jobs, j)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
	}))

	// --- Agent Enrollment & Outbound Tunnel ---
	appMux.HandleFunc("POST /api/agent/enroll", func(w http.ResponseWriter, r *http.Request) {
		if !authLimiter.Allow(audit.ClientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "too many enrollment attempts; please wait a minute")
			return
		}
		var req protocol.EnrollRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		res, err := broker.EnrollNode(r.Context(), req)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, res)
	})

	appMux.HandleFunc("GET /api/agent/tunnel", broker.HandleAgentTunnel)

	// --- Single-Process Production Web UI Serving (OURCHIVE_WEB_DIR, default: web/dist) ---
	if info, err := os.Stat(filepath.Join(webDir, "index.html")); err == nil && !info.IsDir() {
		log.Printf("serving built Web UI from %s", webDir)
		appMux.HandleFunc("/", newSPAHandler(webDir))
	}

	// --- Isolated Share Subdomain (s.localhost / s.yourdomain.com) ---
	shareHandler := shareSvc.ShareHandler()

	rootHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Baseline security headers on all responses.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")

		host := strings.ToLower(strings.Split(r.Host, ":")[0])
		if host == shareHost {
			shareHandler.ServeHTTP(w, r)
			return
		}
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")

		// CSRF defense-in-depth: block cross-site browser mutations on /api/* (excluding CLI agent enrollment).
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/agent/enroll" {
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
				if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
					writeError(w, http.StatusForbidden, "cross-site request blocked")
					return
				}
			}
		}

		appMux.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           rootHandler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("ourchive hub listening on http://localhost%s (share host: %s, db: %s)", addr, shareHost, dbPath)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("hub server error: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	log.Println("ourchive hub stopped")
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func newSPAHandler(webDir string) http.HandlerFunc {
	absRoot, _ := filepath.Abs(webDir)
	indexPath := filepath.Join(absRoot, "index.html")
	fsHandler := http.FileServer(http.Dir(absRoot))

	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		clean := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if clean == "." || clean == "" {
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeFile(w, r, indexPath)
			return
		}
		target := filepath.Join(absRoot, clean)
		if strings.HasPrefix(target, absRoot+string(os.PathSeparator)) {
			if info, err := os.Stat(target); err == nil && !info.IsDir() {
				if strings.HasPrefix(r.URL.Path, "/assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				fsHandler.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, indexPath)
	}
}

type ipBucket struct {
	tokens float64
	last   time.Time
}

type ipRateLimiter struct {
	mu           sync.Mutex
	buckets      map[string]*ipBucket
	ratePerSec   float64
	burst        float64
	lastCleanup  time.Time
}

func newIPRateLimiter(perMinute, burst int) *ipRateLimiter {
	return &ipRateLimiter{
		buckets:     map[string]*ipBucket{},
		ratePerSec:  float64(perMinute) / 60.0,
		burst:       float64(burst),
		lastCleanup: time.Now(),
	}
}

func (l *ipRateLimiter) Allow(ip string) bool {
	if ip == "" {
		ip = "unknown"
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastCleanup) > 10*time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.last) > 15*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.lastCleanup = now
	}
	b, ok := l.buckets[ip]
	if !ok {
		l.buckets[ip] = &ipBucket{tokens: l.burst - 1, last: now}
		return true
	}
	b.tokens += now.Sub(b.last).Seconds() * l.ratePerSec
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens -= 1
	return true
}

func copyThrottled(w io.Writer, r io.Reader, capBytesPerSec int64) {
	buf := make([]byte, 32*1024)
	start := time.Now()
	var total int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			total += int64(n)
			expected := time.Duration(float64(total) / float64(capBytesPerSec) * float64(time.Second))
			if wait := expected - time.Since(start); wait > 0 {
				time.Sleep(wait)
			}
		}
		if err != nil {
			return
		}
	}
}

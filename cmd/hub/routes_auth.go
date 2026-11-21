package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ourchive/ourchive/internal/audit"
	"github.com/ourchive/ourchive/internal/auth"
)

func (h *hubApp) registerAuthRoutes(appMux *http.ServeMux) {
	authSvc := h.authSvc
	settingsStore := h.settingsStore
	auditSvc := h.auditSvc
	oidcMgr := h.oidcMgr
	passkeyMgr := h.passkeyMgr
	authLimiter := h.authLimiter

	// --- Passkeys (WebAuthn) & OIDC ---
	appMux.HandleFunc("GET /api/auth/oidc/providers", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"providers":        settingsStore.PublicOIDCProviders(),
			"passkeys_enabled": passkeyMgr != nil,
		})
	})

	appMux.HandleFunc("GET /api/auth/oidc/{provider}/login", func(w http.ResponseWriter, r *http.Request) {
		providerID := r.PathValue("provider")
		scheme := "http"
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
		cbURL := scheme + "://" + r.Host + "/api/auth/oidc/" + url.PathEscape(providerID) + "/callback"
		invite := strings.TrimSpace(r.URL.Query().Get("invite"))
		authURL, err := oidcMgr.BeginLogin(r.Context(), providerID, cbURL, invite)
		if err != nil {
			http.Redirect(w, r, "/?auth_error="+url.QueryEscape(err.Error()), http.StatusFound)
			return
		}
		http.Redirect(w, r, authURL, http.StatusFound)
	})

	appMux.HandleFunc("GET /api/auth/oidc/{provider}/callback", func(w http.ResponseWriter, r *http.Request) {
		providerID := r.PathValue("provider")
		if errMsg := r.URL.Query().Get("error"); errMsg != "" {
			http.Redirect(w, r, "/?auth_error="+url.QueryEscape(errMsg), http.StatusFound)
			return
		}
		scheme := "http"
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
		cbURL := scheme + "://" + r.Host + "/api/auth/oidc/" + url.PathEscape(providerID) + "/callback"
		u, err := oidcMgr.CompleteCallback(r.Context(), providerID, r.URL.Query().Get("code"), r.URL.Query().Get("state"), cbURL)
		if err != nil {
			http.Redirect(w, r, "/?auth_error="+url.QueryEscape(err.Error()), http.StatusFound)
			return
		}
		sid, exp, err := authSvc.CreateSession(r.Context(), u.ID, auth.DefaultSessionTTL)
		if err != nil {
			http.Redirect(w, r, "/?auth_error="+url.QueryEscape("failed to create session"), http.StatusFound)
			return
		}
		auth.SetSessionCookie(w, r, sid, exp)
		auditSvc.Log(r.Context(), audit.Entry{
			Category:   audit.CategoryCore,
			Action:     "auth.oidc_login",
			ActorID:    u.ID,
			ActorLabel: u.Label(),
			Metadata:   map[string]any{"provider": providerID},
			IPAddress:  audit.ClientIP(r),
		})
		http.Redirect(w, r, "/", http.StatusFound)
	})

	appMux.HandleFunc("POST /api/auth/passkeys/login/begin", func(w http.ResponseWriter, r *http.Request) {
		if passkeyMgr == nil {
			writeError(w, http.StatusServiceUnavailable, "passkeys are not configured")
			return
		}
		assertion, token, err := passkeyMgr.BeginDiscoverableLogin()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"options":        assertion.Response,
			"ceremony_token": token,
		})
	})

	appMux.HandleFunc("POST /api/auth/passkeys/login/finish", func(w http.ResponseWriter, r *http.Request) {
		if passkeyMgr == nil {
			writeError(w, http.StatusServiceUnavailable, "passkeys are not configured")
			return
		}
		if !authLimiter.Allow(audit.ClientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "too many attempts; please wait a minute")
			return
		}
		token := r.URL.Query().Get("token")
		u, err := passkeyMgr.FinishDiscoverableLogin(r.Context(), token, r)
		if errors.Is(err, auth.ErrPasskeyNotFound) {
			writeError(w, http.StatusNotFound, "passkey not found on this hub")
			return
		}
		if err != nil {
			writeError(w, http.StatusUnauthorized, err.Error())
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
			Action:     "auth.passkey_login",
			ActorID:    u.ID,
			ActorLabel: u.Label(),
			IPAddress:  audit.ClientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"user": u})
	})

	appMux.HandleFunc("GET /api/auth/passkeys", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		if passkeyMgr == nil {
			writeJSON(w, http.StatusOK, map[string]any{"rpId": "", "userId": "", "credentials": []any{}})
			return
		}
		u := auth.UserFromContext(r.Context())
		creds, err := passkeyMgr.ListCredentials(r.Context(), u.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"rpId":        passkeyMgr.RPID(),
			"userId":      auth.PasskeyUserID(u.ID),
			"credentials": creds,
		})
	}))

	appMux.HandleFunc("POST /api/auth/passkeys/register/begin", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		if passkeyMgr == nil {
			writeError(w, http.StatusServiceUnavailable, "passkeys are not configured")
			return
		}
		u := auth.UserFromContext(r.Context())
		var req struct {
			Promotion bool `json:"promotion"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		creation, token, err := passkeyMgr.BeginRegistration(r.Context(), u, req.Promotion)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"options":        creation.Response,
			"ceremony_token": token,
		})
	}))

	appMux.HandleFunc("POST /api/auth/passkeys/register/finish", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		if passkeyMgr == nil {
			writeError(w, http.StatusServiceUnavailable, "passkeys are not configured")
			return
		}
		u := auth.UserFromContext(r.Context())
		token := r.URL.Query().Get("token")
		name := r.URL.Query().Get("name")
		cred, err := passkeyMgr.FinishRegistration(r.Context(), u, token, name, r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		auditSvc.Log(r.Context(), audit.Entry{
			Category:   audit.CategoryCore,
			Action:     "auth.passkey_registered",
			ActorID:    u.ID,
			ActorLabel: u.Label(),
			TargetType: "passkey",
			TargetID:   cred.ID,
			IPAddress:  audit.ClientIP(r),
		})
		writeJSON(w, http.StatusCreated, map[string]any{"credential": cred})
	}))

	appMux.HandleFunc("PUT /api/auth/passkeys/{id}", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		if passkeyMgr == nil {
			writeError(w, http.StatusServiceUnavailable, "passkeys are not configured")
			return
		}
		u := auth.UserFromContext(r.Context())
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if err := passkeyMgr.RenameCredential(r.Context(), u.ID, r.PathValue("id"), req.Name); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	appMux.HandleFunc("DELETE /api/auth/passkeys/{id}", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		if passkeyMgr == nil {
			writeError(w, http.StatusServiceUnavailable, "passkeys are not configured")
			return
		}
		u := auth.UserFromContext(r.Context())
		if err := passkeyMgr.DeleteCredential(r.Context(), u.ID, r.PathValue("id")); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	appMux.HandleFunc("PUT /api/auth/profile", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFromContext(r.Context())
		var req struct {
			Username    string `json:"username"`
			DisplayName string `json:"display_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		updated, err := authSvc.UpdateProfile(r.Context(), u.ID, req.Username, req.DisplayName)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		rpID := ""
		if passkeyMgr != nil {
			rpID = passkeyMgr.RPID()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"user":   updated,
			"rpId":   rpID,
			"userId": auth.PasskeyUserID(updated.ID),
		})
	}))

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

}

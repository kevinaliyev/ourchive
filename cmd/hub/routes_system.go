package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ourchive/ourchive/internal/audit"
	"github.com/ourchive/ourchive/internal/auth"
	"github.com/ourchive/ourchive/internal/clientdist"
	"github.com/ourchive/ourchive/internal/settings"
	"github.com/ourchive/ourchive/internal/storage"
)

func (h *hubApp) registerSystemRoutes(appMux *http.ServeMux) {
	store := h.store
	authSvc := h.authSvc
	settingsStore := h.settingsStore
	auditSvc := h.auditSvc
	eventBus := h.eventBus
	shareHost := h.shareHost

	// --- Self-Hosted Agent Binary Downloads (/downloads/*) ---
	// Serves cross-platform ourchive-agent binaries, desktop app installers (ourchive-client-*) and
	// manifest.json from OURCHIVE_DIST_DIR
	// so contributors can install via PowerShell/curl directly from this Hub without needing
	// GitHub Releases. Set OURCHIVE_AGENT_DOWNLOAD_URL later to switch to official signed releases.
	appMux.HandleFunc("GET /downloads/{filename}", func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.PathValue("filename"))
		if name == "." || name == "/" || strings.Contains(name, "..") {
			writeError(w, http.StatusBadRequest, "invalid filename")
			return
		}
		if name != "manifest.json" && !strings.HasPrefix(name, "ourchive-agent-") && !strings.HasPrefix(name, clientdist.FilePrefix) {
			writeError(w, http.StatusNotFound, "file not found")
			return
		}
		fullPath := filepath.Join(h.distDir, name)
		info, err := os.Stat(fullPath)
		if err != nil || info.IsDir() {
			writeError(w, http.StatusNotFound, "binary not available on hub (run scripts/build-agent or set OURCHIVE_AGENT_DOWNLOAD_URL)")
			return
		}
		if name == "manifest.json" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-cache")
		} else {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
			w.Header().Set("Cache-Control", "public, max-age=300")
		}
		http.ServeFile(w, r, fullPath)
	})

	// --- Real-Time SSE Stream ---
	appMux.HandleFunc("GET /api/events", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		eventBus.ServeSSE(w, r)
	}))

	// --- Health ---
	appMux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, healthResponse{
			Status:     "ok",
			Version:    "0.3.0",
			APIVersion: clientAPIVersion,
			HubName:    h.hubName,
			ShareHost:  shareHost,
			Time:       time.Now().UTC().Format(time.RFC3339),
		})
	})

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

}

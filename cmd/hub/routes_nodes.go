package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ourchive/ourchive/internal/audit"
	"github.com/ourchive/ourchive/internal/auth"
	"github.com/ourchive/ourchive/pkg/protocol"
)

func (h *hubApp) registerNodeRoutes(appMux *http.ServeMux) {
	store := h.store
	authSvc := h.authSvc
	broker := h.broker
	auditSvc := h.auditSvc
	eventBus := h.eventBus
	authLimiter := h.authLimiter
	publicHubURL := h.publicHubURL
	agentDownloadURL := h.agentDownloadURL
	agentManifestURL := h.agentManifestURL

	// --- Per-Mount Group ACLs ---
	appMux.HandleFunc("GET /api/acls", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFromContext(r.Context())
		nodeID := r.URL.Query().Get("node_id")
		mountID := r.URL.Query().Get("mount_id")
		if nodeID != "" && u.Role != auth.RoleHostAdmin {
			ok, err := broker.IsNodeOwner(r.Context(), nodeID, u)
			if err != nil || !ok {
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
		} else if nodeID == "" && u.Role != auth.RoleHostAdmin {
			writeError(w, http.StatusForbidden, "node_id is required")
			return
		}
		list, err := authSvc.ListMountACLs(r.Context(), nodeID, mountID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"acls": list})
	}))

	appMux.HandleFunc("PUT /api/acls", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFromContext(r.Context())
		var req auth.MountACL
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		isOwner, err := broker.IsNodeOwner(r.Context(), req.NodeID, u)
		if err != nil || (!isOwner && u.Role != auth.RoleHostAdmin) {
			writeError(w, http.StatusForbidden, "only the machine owner or host admin can manage folder ACLs")
			return
		}
		// Security invariant: even host_admin cannot grant write/delete on another user's node.
		if !isOwner && (req.CanWrite || req.CanDelete) {
			writeError(w, http.StatusForbidden, "only the machine owner can grant write or delete access to their machine")
			return
		}
		saved, err := authSvc.UpsertMountACL(r.Context(), req)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		eventBus.Publish("acls.updated", map[string]any{"node_id": req.NodeID, "mount_id": req.MountID})
		writeJSON(w, http.StatusOK, map[string]any{"acl": saved})
	}))

	appMux.HandleFunc("DELETE /api/acls/{id}", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFromContext(r.Context())
		aclID := r.PathValue("id")
		aclNodeID, err := authSvc.GetMountACLNodeID(r.Context(), aclID)
		if err != nil {
			writeError(w, http.StatusNotFound, "ACL not found")
			return
		}
		if u.Role != auth.RoleHostAdmin {
			ok, err := broker.IsNodeOwner(r.Context(), aclNodeID, u)
			if err != nil || !ok {
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
		}
		if err := authSvc.DeleteMountACL(r.Context(), aclID); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		eventBus.Publish("acls.updated", map[string]any{"id": aclID})
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
		nameFlag := ""
		if trimmedName := strings.TrimSpace(req.DisplayName); trimmedName != "" {
			// Escape double quotes so shell snippets stay well-formed.
			safeName := strings.ReplaceAll(trimmedName, `"`, "")
			nameFlag = ` --name "` + safeName + `"`
		}

		// Default to Hub-hosted binaries (/downloads); automatically switches to official
		// signed releases once OURCHIVE_AGENT_DOWNLOAD_URL is configured.
		downloadBase := publicHubURL + "/downloads"
		binarySource := "hub"
		if agentDownloadURL != "" {
			downloadBase = agentDownloadURL
			binarySource = "official"
		}
		manifestURL := downloadBase + "/manifest.json"
		if agentManifestURL != "" {
			manifestURL = agentManifestURL
		}

		sourceCmd := "go run ./cmd/agent enroll --hub " + publicHubURL + " --token " + tok + nameFlag
		installWindows := `Stop-Process -Name ourchive-agent -Force -ErrorAction SilentlyContinue; $dir="$env:LOCALAPPDATA\Ourchive"; New-Item -ItemType Directory -Force $dir | Out-Null; $exe="$dir\ourchive-agent.exe"; Invoke-WebRequest "` + downloadBase + `/ourchive-agent-windows-amd64.exe" -OutFile $exe; & $exe enroll --hub "` + publicHubURL + `" --token "` + tok + `"` + nameFlag + `; & $exe update --url "` + manifestURL + `" --enable-auto; & $exe service install; Start-Process $exe -ArgumentList "run" -WindowStyle Hidden`
		installUnix := `bash -c 'OS=$(uname -s | tr "[:upper:]" "[:lower:]"); ARCH=$(uname -m | sed -e "s/x86_64/amd64/" -e "s/aarch64/arm64/"); DIR="$HOME/.local/bin"; mkdir -p "$DIR"; BIN="$DIR/ourchive-agent"; pkill -x ourchive-agent 2>/dev/null || true; rm -f "$BIN"; curl -fsSL "` + downloadBase + `/ourchive-agent-$OS-$ARCH" -o "$BIN" && chmod +x "$BIN" && "$BIN" enroll --hub "` + publicHubURL + `" --token "` + tok + `"` + nameFlag + ` && "$BIN" update --url "` + manifestURL + `" --enable-auto && "$BIN" service install && nohup "$BIN" run >/dev/null 2>&1 &'`

		writeJSON(w, http.StatusCreated, map[string]any{
			"token":             tok,
			"expires_at":        exp.Format(time.RFC3339),
			"command":           sourceCmd,
			"install_windows":   installWindows,
			"install_unix":      installUnix,
			"download_base_url": downloadBase,
			"manifest_url":      manifestURL,
			"binary_source":     binarySource,
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
		var dbSourceNodeID string
		var allowSync int
		if err := store.DB.QueryRowContext(r.Context(), `SELECT node_id, allow_sync FROM mounts WHERE id = ?`, req.SourceMountID).Scan(&dbSourceNodeID, &allowSync); err != nil || allowSync != 1 {
			writeError(w, http.StatusForbidden, "contributor has not opted this folder into parity sync")
			return
		}
		if isSrcOwner, _ := broker.IsNodeOwner(r.Context(), dbSourceNodeID, u); !isSrcOwner {
			if allowed, _ := authSvc.CheckMountScope(r.Context(), u.ID, dbSourceNodeID, req.SourceMountID, "read"); !allowed {
				writeError(w, http.StatusForbidden, "you do not have read access to this folder")
				return
			}
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
		`, id, u.ID, dbSourceNodeID, req.SourceMountID, req.TargetNodeID, strings.TrimSpace(req.TargetPath), time.Now().UTC().Format(time.RFC3339))
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

}

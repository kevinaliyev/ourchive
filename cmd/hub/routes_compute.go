package main

import (
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/ourchive/ourchive/internal/auth"
	"github.com/ourchive/ourchive/internal/tunnel"
	"github.com/ourchive/ourchive/pkg/protocol"
)

func (h *hubApp) registerComputeRoutes(appMux *http.ServeMux) {
	store := h.store
	authSvc := h.authSvc
	broker := h.broker

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

}

package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ourchive/ourchive/internal/audit"
	"github.com/ourchive/ourchive/internal/auth"
	"github.com/ourchive/ourchive/internal/tunnel"
	"github.com/ourchive/ourchive/internal/vfs"
	"github.com/ourchive/ourchive/pkg/protocol"
)

func (h *hubApp) registerVFSRoutes(appMux *http.ServeMux) {
	authSvc := h.authSvc
	broker := h.broker
	tagStore := h.tagStore
	settingsStore := h.settingsStore
	auditSvc := h.auditSvc
	thumbDir := h.thumbDir

	// --- Phase 2: Virtual Filesystem (Proxy over yamux) ---
	appMux.HandleFunc("GET /api/vfs/nodes/{nodeId}/mounts/{mountId}/list", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		nodeID := r.PathValue("nodeId")
		mountID := r.PathValue("mountId")
		u := auth.UserFromContext(r.Context())
		if isOwner, _ := broker.IsNodeOwner(r.Context(), nodeID, u); !isOwner {
			if allowed, _ := authSvc.CheckMountScope(r.Context(), u.ID, nodeID, mountID, "read"); !allowed {
				writeError(w, http.StatusForbidden, "you do not have access to this folder")
				return
			}
		}
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

	appMux.HandleFunc("POST /api/vfs/nodes/{nodeId}/mounts/{mountId}/rename", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		nodeID := r.PathValue("nodeId")
		mountID := r.PathValue("mountId")
		u := auth.UserFromContext(r.Context())
		ok, err := broker.IsNodeOwner(r.Context(), nodeID, u)
		if err != nil || !ok {
			if allowed, _ := authSvc.CheckMountScope(r.Context(), u.ID, nodeID, mountID, "write"); !allowed {
				writeError(w, http.StatusForbidden, "you do not have write permission on this folder")
				return
			}
		}
		client, err := broker.HTTPClientForNode(nodeID)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "machine is offline")
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://agent/vfs/"+mountID+"/rename", strings.NewReader(string(body)))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			writeError(w, http.StatusBadGateway, "rename failed")
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))

	appMux.HandleFunc("GET /api/vfs/search", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFromContext(r.Context())
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if q == "" {
			writeJSON(w, http.StatusOK, map[string]any{"hits": []any{}})
			return
		}
		nodes, err := broker.ListNodesForUser(r.Context(), u)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list nodes")
			return
		}
		type globalHit struct {
			NodeID    string `json:"node_id"`
			NodeName  string `json:"node_name"`
			IsOwnHost bool   `json:"is_own_host"`
			MountID   string `json:"mount_id"`
			MountName string `json:"mount_name"`
			Name      string `json:"name"`
			Path      string `json:"path"`
			IsDir     bool   `json:"is_dir"`
			Size      int64  `json:"size"`
			ModTime   string `json:"mod_time"`
			MimeType  string `json:"mime_type"`
		}
		var mu sync.Mutex
		var wg sync.WaitGroup
		results := make([]globalHit, 0, 32)
		for _, n := range nodes {
			if n.Status != protocol.NodeStatusOnline {
				continue
			}
			wg.Add(1)
			go func(node tunnel.NodeView) {
				defer wg.Done()
				cli, err := broker.HTTPClientForNode(node.ID)
				if err != nil {
					return
				}
				params := url.Values{}
				params.Set("q", q)
				params.Set("limit", "40")
				req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://agent/vfs/search?"+params.Encode(), nil)
				if err != nil {
					return
				}
				resp, err := cli.Do(req)
				if err != nil || resp.StatusCode != http.StatusOK {
					if resp != nil {
						resp.Body.Close()
					}
					return
				}
				defer resp.Body.Close()
				var data struct {
					Hits []vfs.SearchHit `json:"hits"`
				}
				if json.NewDecoder(resp.Body).Decode(&data) != nil {
					return
				}
				for _, h := range data.Hits {
					if !node.IsOwner {
						if allowed, _ := authSvc.CheckMountScope(r.Context(), u.ID, node.ID, h.MountID, "read"); !allowed {
							continue
						}
					}
					mu.Lock()
					results = append(results, globalHit{
						NodeID:    node.ID,
						NodeName:  node.DisplayName,
						IsOwnHost: node.IsOwner,
						MountID:   h.MountID,
						MountName: h.MountName,
						Name:      h.Name,
						Path:      h.Path,
						IsDir:     h.IsDir,
						Size:      h.Size,
						ModTime:   h.ModTime,
						MimeType:  h.MimeType,
					})
					mu.Unlock()
				}
			}(n)
		}
		wg.Wait()
		writeJSON(w, http.StatusOK, map[string]any{"hits": results})
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
		nodeID := r.PathValue("nodeId")
		u := auth.UserFromContext(r.Context())
		canWrite, _ := broker.IsNodeOwner(r.Context(), nodeID, u)
		client, err := broker.HTTPClientForNode(nodeID)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "machine is offline")
			return
		}
		payload, _ := json.Marshal(map[string]any{
			"sdp":          req.SDP,
			"stun_servers": pol.STUNServers,
			"can_write":    canWrite,
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

}

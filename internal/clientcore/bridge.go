package clientcore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// bridgeHeader must be present on every mutating bridge call. A custom header cannot be sent
// cross-origin without a CORS preflight, which the bridge never grants.
const bridgeHeader = "X-Ourchive-Client"

func (c *Core) bridgeMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_client/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, c.Snapshot())
	})
	mux.HandleFunc("GET /_client/events", c.serveEvents)

	mux.HandleFunc("POST /_client/hubs", c.mutating(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			URL      string `json:"url"`
			Name     string `json:"name"`
			Activate bool   `json:"activate"`
			Force    bool   `json:"force"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		norm, err := NormalizeHubURL(req.URL)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		info := c.probeHub(Hub{ID: hubID(norm), URL: norm, Name: defaultHubName(norm)})
		if !info.Reachable && !req.Force {
			writeJSONError(w, http.StatusBadRequest, "could not verify "+norm+": "+info.Error)
			return
		}
		name := req.Name
		if strings.TrimSpace(name) == "" {
			name = info.Name
		}
		hub, err := c.hubs.Add(name, norm)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if req.Activate {
			_ = c.hubs.SetActive(hub.ID)
		}
		c.changed()
		writeJSON(w, http.StatusCreated, hub)
	}))

	mux.HandleFunc("DELETE /_client/hubs/{id}", c.mutating(func(w http.ResponseWriter, r *http.Request) {
		if err := c.hubs.Remove(r.PathValue("id")); err != nil {
			writeJSONError(w, statusFor(err), err.Error())
			return
		}
		c.changed()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))

	mux.HandleFunc("POST /_client/hubs/{id}/activate", c.mutating(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := c.hubs.SetActive(id); err != nil {
			writeJSONError(w, statusFor(err), err.Error())
			return
		}
		if hub, ok := c.hubs.Get(id); ok {
			c.probeHubAsync(hub)
		}
		c.changed()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))

	mux.HandleFunc("POST /_client/hubs/{id}/sign-in", c.mutating(func(w http.ResponseWriter, r *http.Request) {
		if err := c.BeginBrowserSignIn(r.PathValue("id")); err != nil {
			writeJSONError(w, statusFor(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))

	mux.HandleFunc("POST /_client/open-external", c.mutating(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			URL string `json:"url"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		u, err := url.Parse(strings.TrimSpace(req.URL))
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			writeJSONError(w, http.StatusBadRequest, "only http(s) links can be opened")
			return
		}
		if err := c.shell.OpenExternal(u.String()); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))

	mux.HandleFunc("POST /_client/download", c.mutating(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Path     string `json:"path"`
			Filename string `json:"filename"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		d, err := c.StartDownload(req.Path, req.Filename)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"started": d != nil, "download": d})
	}))

	mux.HandleFunc("POST /_client/save-blob", c.mutating(func(w http.ResponseWriter, r *http.Request) {
		path, err := c.SaveBlob(r.Context(), r.URL.Query().Get("filename"), r.Body)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if path != "" {
			c.shell.Notify("Saved", path)
		}
		writeJSON(w, http.StatusOK, map[string]any{"saved": path != "", "path": path})
	}))

	mux.HandleFunc("POST /_client/agent/retry", c.mutating(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			HubURL string `json:"hub_url"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": c.RetryAgent(req.HubURL)})
	}))

	mux.HandleFunc("POST /_client/agent/enroll", c.mutating(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token    string `json:"token"`
			Name     string `json:"name"`
			ReadOnly bool   `json:"read_only"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		if err := c.EnrollActiveHub(r.Context(), req.Token, req.Name, req.ReadOnly); err != nil {
			writeJSONError(w, statusFor(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))

	mux.HandleFunc("POST /_client/agent/enrollment", c.mutating(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Accept   bool   `json:"accept"`
			Name     string `json:"name"`
			ReadOnly bool   `json:"read_only"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		if err := c.ConfirmEnrollment(r.Context(), req.Accept, req.Name, req.ReadOnly); err != nil {
			writeJSONError(w, statusFor(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))

	mux.HandleFunc("/_client/", func(w http.ResponseWriter, r *http.Request) {
		writeJSONError(w, http.StatusNotFound, "unknown client endpoint")
	})
	return mux
}

// mutating requires the bridge header on state-changing calls.
func (c *Core) mutating(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(bridgeHeader) == "" {
			writeJSONError(w, http.StatusForbidden, "missing "+bridgeHeader+" header")
			return
		}
		next(w, r)
	}
}

// serveEvents streams the full State as a "state" event after every change.
func (c *Core) serveEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	updates, unsubscribe := c.Subscribe()
	defer unsubscribe()
	send := func() bool {
		data, _ := json.Marshal(c.Snapshot())
		if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !send() {
		return
	}
	keepAlive := time.NewTicker(25 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-c.ctx.Done():
			return
		case <-keepAlive.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-updates:
			if !send() {
				return
			}
		}
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(v); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrUnknownHub):
		return http.StatusNotFound
	case errors.Is(err, ErrContributorUnsupported):
		return http.StatusNotImplemented
	default:
		return http.StatusBadRequest
	}
}

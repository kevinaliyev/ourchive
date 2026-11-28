package clientcore

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// bridgeHeader must be present on every mutating bridge call. A custom header cannot be sent
// cross-origin without a CORS preflight, which the bridge never grants.
const bridgeHeader = "X-Ourchive-Client"

// longPollTimeout bounds GET /_client/state?since=… so idle connections are recycled.
const longPollTimeout = 25 * time.Second

func (c *Core) bridgeMux() http.Handler {
	mux := http.NewServeMux()
	// GET /_client/state returns the state at once. With ?since=<version> it long-polls: it waits
	// (up to 25s) for a newer version. Long-polling rather than server-sent events, because not
	// every webview can stream a response (WebView2 under Wails buffers until the handler returns).
	mux.HandleFunc("GET /_client/state", func(w http.ResponseWriter, r *http.Request) {
		since := r.URL.Query().Get("since")
		if since == "" {
			writeJSON(w, http.StatusOK, c.Snapshot())
			return
		}
		n, err := strconv.ParseUint(since, 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "since must be a state version")
			return
		}
		writeJSON(w, http.StatusOK, c.WaitForChange(r.Context(), n, longPollTimeout))
	})

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

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/ourchive/ourchive/internal/audit"
	"github.com/ourchive/ourchive/internal/auth"
	"github.com/ourchive/ourchive/internal/clientdist"
	"github.com/ourchive/ourchive/internal/handoff"
)

// clientAPIVersion is reported by /api/health so the desktop client, which bundles its own copy of
// the web UI, can tell when it is talking to a hub it does not understand. Bump it only for
// breaking API changes.
const clientAPIVersion = 1

// clientAuthScheme is the URL scheme the desktop client registers with the OS.
const clientAuthScheme = "ourchive"

// registerClientRoutes adds the desktop client's browser sign-in handoff:
//
//  1. The client opens https://<hub>/?client_auth=<S256 challenge>&client_state=<state> in the
//     system browser. The web UI signs the user in with any method (password, passkey, OIDC),
//     shows a consent card, and POSTs /api/auth/client/authorize.
//  2. The hub answers with an ourchive://auth?code=…&state=… link that the browser hands to the
//     client.
//  3. The client POSTs /api/auth/client/exchange with the code and its PKCE verifier and gets a
//     new session of its own.
func (h *hubApp) registerClientRoutes(appMux *http.ServeMux) {
	if h.clientHandoff == nil {
		h.clientHandoff = handoff.NewStore(handoff.DefaultTTL)
	}
	if h.clientDist == nil {
		h.clientDist = clientdist.New(clientdist.Options{DistDir: h.distDir})
	}
	authSvc := h.authSvc
	auditSvc := h.auditSvc
	store := h.clientHandoff

	// Where to download the desktop app: the newest client release of the configured GitHub
	// repository, else installers placed in this hub's dist directory. The website uses it for the
	// "Get the app" buttons; it never needs to know which source answered.
	appMux.HandleFunc("GET /api/client/download", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, max-age=60")
		writeJSON(w, http.StatusOK, h.clientDist.Resolve(r.Context()))
	}))

	appMux.HandleFunc("POST /api/auth/client/authorize", authSvc.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Challenge string `json:"challenge"`
			State     string `json:"state"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if !handoff.ValidState(req.State) {
			writeError(w, http.StatusBadRequest, handoff.ErrInvalidState.Error())
			return
		}
		u := auth.UserFromContext(r.Context())
		code, err := store.Issue(u.ID, req.Challenge)
		if errors.Is(err, handoff.ErrInvalidChallenge) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, handoff.ErrTooManyPending) {
			writeError(w, http.StatusTooManyRequests, err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to issue sign-in code")
			return
		}
		q := url.Values{}
		q.Set("code", code)
		q.Set("state", req.State)
		q.Set("hub", h.publicHubURL)
		auditSvc.Log(r.Context(), audit.Entry{
			Category:   audit.CategoryCore,
			Action:     "auth.client_authorize",
			ActorID:    u.ID,
			ActorLabel: u.Label(),
			IPAddress:  audit.ClientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]string{
			"redirect_url": clientAuthScheme + "://auth?" + q.Encode(),
		})
	}))

	appMux.HandleFunc("POST /api/auth/client/exchange", func(w http.ResponseWriter, r *http.Request) {
		if !h.authLimiter.Allow(audit.ClientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "too many attempts; please wait a minute")
			return
		}
		var req struct {
			Code     string `json:"code"`
			Verifier string `json:"verifier"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		userID, err := store.Redeem(strings.TrimSpace(req.Code), strings.TrimSpace(req.Verifier))
		if err != nil {
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		u, err := authSvc.GetUserByID(r.Context(), userID)
		if err != nil || u == nil {
			writeError(w, http.StatusUnauthorized, "user no longer exists")
			return
		}
		sid, exp, err := authSvc.CreateSession(r.Context(), u.ID, auth.DefaultSessionTTL)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create session")
			return
		}
		auditSvc.Log(r.Context(), audit.Entry{
			Category:   audit.CategoryCore,
			Action:     "auth.client_login",
			ActorID:    u.ID,
			ActorLabel: u.Label(),
			IPAddress:  audit.ClientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"session_cookie": auth.SessionCookieName,
			"session_token":  sid,
			"expires_at":     exp.UTC(),
			"user":           u,
		})
	})
}

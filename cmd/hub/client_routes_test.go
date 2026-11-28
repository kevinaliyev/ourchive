package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ourchive/ourchive/internal/auth"
	"github.com/ourchive/ourchive/internal/handoff"
)

const testVerifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
const testState = "client-state-0123456789"

func authorizeClient(t *testing.T, h http.Handler, cookie *http.Cookie) (code string) {
	t.Helper()
	rec := doReq(t, h, http.MethodPost, "/api/auth/client/authorize", cookie, map[string]string{
		"challenge": handoff.Challenge(testVerifier),
		"state":     testState,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("authorize: %d %s", rec.Code, rec.Body.String())
	}
	var res struct {
		RedirectURL string `json:"redirect_url"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	u, err := url.Parse(res.RedirectURL)
	if err != nil || u.Scheme != "ourchive" || u.Host != "auth" {
		t.Fatalf("unexpected redirect %q", res.RedirectURL)
	}
	if u.Query().Get("state") != testState || u.Query().Get("hub") != "http://localhost:8080" {
		t.Fatalf("redirect must echo state and name the hub: %q", res.RedirectURL)
	}
	code = u.Query().Get("code")
	if code == "" {
		t.Fatalf("redirect has no code: %q", res.RedirectURL)
	}
	return code
}

func TestClientBrowserSignInHandoff(t *testing.T) {
	ctx := context.Background()
	app, h := setupTestHub(t)
	user, err := app.authSvc.SetupHostAdmin(ctx, "ru@example.com", "ru", "Ru", "password1234")
	if err != nil {
		t.Fatal(err)
	}
	browserCookie := makeSessionCookie(t, app.authSvc, user.ID)

	// Authorizing requires a signed-in browser session and well-formed PKCE inputs.
	if rec := doReq(t, h, http.MethodPost, "/api/auth/client/authorize", nil, map[string]string{
		"challenge": handoff.Challenge(testVerifier), "state": testState,
	}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous authorize: got %d, want 401", rec.Code)
	}
	if rec := doReq(t, h, http.MethodPost, "/api/auth/client/authorize", browserCookie, map[string]string{
		"challenge": "not-a-sha256-digest", "state": testState,
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed challenge: got %d, want 400", rec.Code)
	}
	if rec := doReq(t, h, http.MethodPost, "/api/auth/client/authorize", browserCookie, map[string]string{
		"challenge": handoff.Challenge(testVerifier), "state": "x",
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("short state: got %d, want 400", rec.Code)
	}

	// A wrong verifier fails and burns the code.
	code := authorizeClient(t, h, browserCookie)
	if rec := doReq(t, h, http.MethodPost, "/api/auth/client/exchange", nil, map[string]string{
		"code": code, "verifier": strings.Repeat("z", 43),
	}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong verifier: got %d, want 401", rec.Code)
	}
	if rec := doReq(t, h, http.MethodPost, "/api/auth/client/exchange", nil, map[string]string{
		"code": code, "verifier": testVerifier,
	}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("burned code: got %d, want 401", rec.Code)
	}

	// The happy path yields a new, working session distinct from the browser's.
	code = authorizeClient(t, h, browserCookie)
	rec := doReq(t, h, http.MethodPost, "/api/auth/client/exchange", nil, map[string]string{
		"code": code, "verifier": testVerifier,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("exchange: %d %s", rec.Code, rec.Body.String())
	}
	var ex struct {
		SessionCookie string `json:"session_cookie"`
		SessionToken  string `json:"session_token"`
		User          struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ex)
	if ex.SessionCookie != auth.SessionCookieName || ex.SessionToken == "" || ex.SessionToken == browserCookie.Value || ex.User.ID != user.ID {
		t.Fatalf("unexpected exchange response: %s", rec.Body.String())
	}
	status := doReq(t, h, http.MethodGet, "/api/auth/status", &http.Cookie{Name: ex.SessionCookie, Value: ex.SessionToken}, nil)
	if !strings.Contains(status.Body.String(), user.ID) {
		t.Fatalf("client session does not authenticate: %s", status.Body.String())
	}

	// Codes are single use.
	if rec := doReq(t, h, http.MethodPost, "/api/auth/client/exchange", nil, map[string]string{
		"code": code, "verifier": testVerifier,
	}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("replayed code: got %d, want 401", rec.Code)
	}
}

func TestHealthReportsClientAPIVersion(t *testing.T) {
	_, h := setupTestHub(t)
	rec := doReq(t, h, http.MethodGet, "/api/health", nil, nil)
	var res struct {
		APIVersion int `json:"api_version"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.APIVersion != clientAPIVersion || clientAPIVersion < 1 {
		t.Fatalf("api_version = %d, want %d", res.APIVersion, clientAPIVersion)
	}
}

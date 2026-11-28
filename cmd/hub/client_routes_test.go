package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ourchive/ourchive/internal/auth"
	"github.com/ourchive/ourchive/internal/clientdist"
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

func TestClientDownloadEndpoint(t *testing.T) {
	ctx := context.Background()
	app, h := setupTestHub(t)
	user, err := app.authSvc.SetupHostAdmin(ctx, "ru@example.com", "ru", "Ru", "password1234")
	if err != nil {
		t.Fatal(err)
	}
	cookie := makeSessionCookie(t, app.authSvc, user.ID)

	if rec := doReq(t, h, http.MethodGet, "/api/client/download", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: got %d, want 401", rec.Code)
	}

	type response struct {
		Source string `json:"source"`
		Assets []struct {
			OS  string `json:"os"`
			URL string `json:"url"`
		} `json:"assets"`
	}
	get := func() response {
		t.Helper()
		rec := doReq(t, h, http.MethodGet, "/api/client/download", cookie, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
		}
		var res response
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		return res
	}

	// Nothing hosted yet: the UI gets an explicit "none" and an empty list, not null.
	if res := get(); res.Source != "none" || res.Assets == nil || len(res.Assets) != 0 {
		t.Fatalf("empty hub: %+v", res)
	}
	if !strings.Contains(doReq(t, h, http.MethodGet, "/api/client/download", cookie, nil).Body.String(), `"assets":[]`) {
		t.Fatal("assets must serialize as [] so the UI can map over it")
	}

	// Installers dropped in the dist dir are offered and served from /downloads.
	dist := t.TempDir()
	app.distDir = dist
	app.clientDist = clientdist.New(clientdist.Options{DistDir: dist})
	for name, body := range map[string]string{
		"ourchive-client-windows-amd64.exe": "MZ-installer",
		"ourchive-agent-windows-amd64.exe":  "agent",
		"secret.txt":                        "nope",
	} {
		if err := os.WriteFile(filepath.Join(dist, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res := get()
	if res.Source != "hub" || len(res.Assets) != 1 || res.Assets[0].OS != "windows" ||
		res.Assets[0].URL != "/downloads/ourchive-client-windows-amd64.exe" {
		t.Fatalf("hub installers: %+v", res)
	}
	if rec := doReq(t, h, http.MethodGet, "/downloads/ourchive-client-windows-amd64.exe", nil, nil); rec.Code != http.StatusOK || rec.Body.String() != "MZ-installer" {
		t.Fatalf("installer download: %d %q", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, h, http.MethodGet, "/downloads/ourchive-agent-windows-amd64.exe", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("agent download must keep working: %d", rec.Code)
	}
	if rec := doReq(t, h, http.MethodGet, "/downloads/secret.txt", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("other files must stay private: %d", rec.Code)
	}
}

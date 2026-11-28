package clientcore

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ourchive/ourchive/internal/agentstatus"
	"github.com/ourchive/ourchive/internal/handoff"
)

// ---- fakes ----

type fakeShell struct {
	mu       sync.Mutex
	opened   []string
	notes    []string
	saveTo   string
	shown    int
	cancelDl bool
}

func (s *fakeShell) OpenExternal(u string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opened = append(s.opened, u)
	return nil
}
func (s *fakeShell) SaveFileDialog(name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelDl {
		return "", nil
	}
	return filepath.Join(s.saveTo, name), nil
}
func (s *fakeShell) Notify(title, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes = append(s.notes, title+": "+body)
}
func (s *fakeShell) ShowWindow() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shown++
}
func (s *fakeShell) lastOpened() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.opened) == 0 {
		return ""
	}
	return s.opened[len(s.opened)-1]
}

type fakeAgent struct {
	board *agentstatus.Board
	mu    sync.Mutex
	added []string
	retry []string
}

func newFakeAgent() *fakeAgent                       { return &fakeAgent{board: agentstatus.NewBoard()} }
func (a *fakeAgent) Status() []agentstatus.HubStatus { return a.board.Snapshot() }
func (a *fakeAgent) Subscribe() (<-chan []agentstatus.HubStatus, func()) {
	return a.board.Subscribe()
}
func (a *fakeAgent) Retry(u string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.retry = append(a.retry, u)
	return true
}
func (a *fakeAgent) AddHub(p string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.added = append(a.added, p)
	return nil
}

// fakeHub is a minimal stand-in for an Ourchive hub.
type fakeHub struct {
	*httptest.Server
	mu        sync.Mutex
	seen      http.Header
	challenge string
	release   chan struct{}
}

func newFakeHub(t *testing.T, apiVersion int) *fakeHub {
	h := &fakeHub{release: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "version": "0.3.0", "hub_name": "Test Hub", "api_version": apiVersion})
	})
	mux.HandleFunc("POST /api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "sid-password", Path: "/", HttpOnly: true, Expires: time.Now().Add(time.Hour)})
		http.SetCookie(w, &http.Cookie{Name: "unrelated", Value: "x"})
		_, _ = io.WriteString(w, `{"user":{"id":"u1"}}`)
	})
	mux.HandleFunc("POST /api/body-echo", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]string{"body": string(b)})
	})
	mux.HandleFunc("POST /api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1})
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("GET /api/echo", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.seen = r.Header.Clone()
		h.mu.Unlock()
		ck, _ := r.Cookie(sessionCookieName)
		val := ""
		if ck != nil {
			val = ck.Value
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"session": val, "host": r.Host})
	})
	mux.HandleFunc("GET /api/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, h.URL+"/?auth_error=nope", http.StatusFound)
	})
	mux.HandleFunc("GET /api/idp", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://idp.example.com/authorize?x=1", http.StatusFound)
	})
	mux.HandleFunc("GET /api/page", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<script>evil()</script>")
	})
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: hello\ndata: 1\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-h.release:
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("GET /api/vfs/video", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "clip.mp4", time.Time{}, strings.NewReader(strings.Repeat("v", 20<<20)))
	})
	mux.HandleFunc("GET /api/vfs/file", func(w http.ResponseWriter, r *http.Request) {
		if ck, _ := r.Cookie(sessionCookieName); ck == nil || ck.Value == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Length", "11")
		_, _ = io.WriteString(w, "hello world")
	})
	mux.HandleFunc("POST /api/auth/client/exchange", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Code, Verifier string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.mu.Lock()
		ch := h.challenge
		h.challenge = "" // single use
		h.mu.Unlock()
		if req.Code != "good-code" || ch == "" || handoff.Challenge(req.Verifier) != ch {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid or expired sign-in code"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session_cookie": sessionCookieName, "session_token": "sid-browser",
			"expires_at": time.Now().Add(time.Hour).UTC(),
		})
	})
	h.Server = httptest.NewServer(mux)
	t.Cleanup(func() {
		select {
		case <-h.release:
		default:
			close(h.release)
		}
		h.Close()
	})
	return h
}

type harness struct {
	core   *Core
	shell  *fakeShell
	agent  *fakeAgent
	app    *httptest.Server
	enroll []EnrollRequest
	mu     sync.Mutex
}

func newHarness(t *testing.T, withAgent bool) *harness {
	t.Helper()
	h := &harness{shell: &fakeShell{saveTo: t.TempDir()}}
	opts := Options{StatePath: filepath.Join(t.TempDir(), "client", "state.json"), Version: "test", Shell: h.shell}
	if withAgent {
		h.agent = newFakeAgent()
		opts.Agent = h.agent
		opts.Enroll = func(ctx context.Context, req EnrollRequest) (string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.enroll = append(h.enroll, req)
			return "/tmp/hubs/" + req.Token + ".json", nil
		}
	}
	core, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(core.Close)
	h.core = core
	assets := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "<!doctype html>app") })
	h.app = httptest.NewServer(core.Handler(assets))
	t.Cleanup(h.app.Close)
	return h
}

func (h *harness) do(t *testing.T, method, path string, body any, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, h.app.URL+path, rd)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

var bridge = map[string]string{bridgeHeader: "1"}

func (h *harness) addHub(t *testing.T, hubURL string) Hub {
	t.Helper()
	resp, data := h.do(t, http.MethodPost, "/_client/hubs", map[string]any{"url": hubURL, "activate": true}, bridge)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add hub: %d %s", resp.StatusCode, data)
	}
	var hub Hub
	_ = json.Unmarshal(data, &hub)
	return hub
}

// ---- tests ----

func TestProxyHoldsSessionLikeACookieJar(t *testing.T) {
	hub := newFakeHub(t, APIVersion)
	h := newHarness(t, false)

	if resp, _ := h.do(t, http.MethodGet, "/api/echo", nil, nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 before any hub is saved, got %d", resp.StatusCode)
	}
	saved := h.addHub(t, hub.URL+"/some/path?x=1")
	if saved.URL != hub.URL || saved.Name != "Test Hub" {
		t.Fatalf("hub should be normalized and named from /api/health, got %+v", saved)
	}

	resp, _ := h.do(t, http.MethodPost, "/api/auth/login", map[string]string{"identifier": "ru", "password": "pw"}, nil)
	if resp.StatusCode != http.StatusOK || len(resp.Header.Values("Set-Cookie")) != 0 {
		t.Fatalf("login must succeed without leaking cookies to the webview: %d %v", resp.StatusCode, resp.Header.Values("Set-Cookie"))
	}
	if tok, ok := h.core.hubs.Session(saved.ID); !ok || tok != "sid-password" {
		t.Fatalf("expected session captured from Set-Cookie, got %q %v", tok, ok)
	}

	_, data := h.do(t, http.MethodGet, "/api/echo", nil, map[string]string{
		"Cookie": "ourchive_session=webview-forged", "Origin": "wails://wails", "Sec-Fetch-Site": "same-origin",
	})
	var echo map[string]string
	_ = json.Unmarshal(data, &echo)
	if echo["session"] != "sid-password" {
		t.Fatalf("proxy must send the stored session, not the webview's cookie; hub saw %q", echo["session"])
	}
	if u, _ := url.Parse(hub.URL); echo["host"] != u.Host {
		t.Fatalf("Host header should be the hub's, got %q", echo["host"])
	}
	hub.mu.Lock()
	seen := hub.seen
	hub.mu.Unlock()
	if seen.Get("Origin") != "" || seen.Get("Sec-Fetch-Site") != "" || seen.Get("X-Ourchive-Client") != "test" {
		t.Fatalf("unexpected forwarded headers: %v", seen)
	}

	if !h.core.Snapshot().Hubs[0].SignedIn {
		t.Fatalf("state should report the hub as signed in")
	}
	h.do(t, http.MethodPost, "/api/auth/logout", nil, nil)
	if _, ok := h.core.hubs.Session(saved.ID); ok {
		t.Fatalf("logout's expired cookie should clear the stored session")
	}
}

func TestOriginChecksAndBridgeHeader(t *testing.T) {
	hub := newFakeHub(t, APIVersion)
	h := newHarness(t, false)
	h.addHub(t, hub.URL)

	for _, site := range []string{"cross-site", "same-site"} {
		if resp, _ := h.do(t, http.MethodGet, "/api/echo", nil, map[string]string{"Sec-Fetch-Site": site}); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("proxy must reject Sec-Fetch-Site=%s, got %d", site, resp.StatusCode)
		}
		if resp, _ := h.do(t, http.MethodGet, "/_client/state", nil, map[string]string{"Sec-Fetch-Site": site}); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("bridge must reject Sec-Fetch-Site=%s, got %d", site, resp.StatusCode)
		}
	}
	if resp, _ := h.do(t, http.MethodPost, "/_client/hubs", map[string]any{"url": hub.URL}, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bridge mutation without %s must be rejected, got %d", bridgeHeader, resp.StatusCode)
	}
	if resp, _ := h.do(t, http.MethodPost, "/_client/open-external", map[string]string{"url": "file:///etc/passwd"}, bridge); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-http links must be refused, got %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, http.MethodPost, "/_client/hubs", map[string]any{"url": "https://127.0.0.1:1"}, bridge); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unreachable hub should not be saved without force, got %d", resp.StatusCode)
	}
	if resp, data := h.do(t, http.MethodGet, "/", nil, nil); string(data) != "<!doctype html>app" || resp.StatusCode != 200 {
		t.Fatalf("non-API paths must fall through to the UI assets")
	}
}

// WebView2 does not send a Content-Length for fetch() bodies, so the request Wails hands the app has
// ContentLength 0 with a body attached. httputil.ReverseProxy treats 0 as "no body" and would send
// an empty request, which the hub rejects as "invalid JSON body".
func TestProxyKeepsBodiesWhenWebViewOmitsContentLength(t *testing.T) {
	hub := newFakeHub(t, APIVersion)
	h := newHarness(t, false)
	h.addHub(t, hub.URL)
	handler := h.core.Handler(http.NotFoundHandler())

	send := func(body io.Reader) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/body-echo", body)
		req.ContentLength = 0 // what the webview's request looks like
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
		}
		var out map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out["body"]
	}

	payload := `{"identifier":"ru","password":"pw"}`
	if got := send(io.NopCloser(strings.NewReader(payload))); got != payload {
		t.Fatalf("hub received %q, want the login body", got)
	}
	big := strings.Repeat("x", 3<<20)
	if got := send(io.NopCloser(strings.NewReader(big))); got != big {
		t.Fatalf("large body was altered: got %d bytes, want %d", len(got), len(big))
	}
	if got := send(http.NoBody); got != "" {
		t.Fatalf("an empty POST must stay empty, hub received %q", got)
	}
	if got := send(io.NopCloser(strings.NewReader(""))); got != "" {
		t.Fatalf("an empty streamed POST must stay empty, hub received %q", got)
	}
}

func TestProxyRewritesRedirectsAndSandboxesDocuments(t *testing.T) {
	hub := newFakeHub(t, APIVersion)
	h := newHarness(t, false)
	h.addHub(t, hub.URL)

	resp, _ := h.do(t, http.MethodGet, "/api/redirect", nil, nil)
	if loc := resp.Header.Get("Location"); loc != "/?auth_error=nope" {
		t.Fatalf("same-hub redirect should become relative, got %q", loc)
	}
	resp, _ = h.do(t, http.MethodGet, "/api/idp", nil, nil)
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "https://idp.example.com/") {
		t.Fatalf("foreign redirect must be left alone, got %q", loc)
	}
	resp, _ = h.do(t, http.MethodGet, "/api/page", nil, nil)
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Fatalf("hub HTML must be sandboxed in the app origin, got CSP %q", csp)
	}
}

func TestProxyStreamsServerSentEvents(t *testing.T) {
	hub := newFakeHub(t, APIVersion)
	h := newHarness(t, false)
	h.addHub(t, hub.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.app.URL+"/api/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "event: hello\n" {
		t.Fatalf("expected the first SSE event while the stream is still open, got %q (%v)", line, err)
	}
}

func TestBrowserSignInHandoff(t *testing.T) {
	hub := newFakeHub(t, APIVersion)
	h := newHarness(t, false)
	saved := h.addHub(t, hub.URL)

	if resp, data := h.do(t, http.MethodPost, "/_client/hubs/"+saved.ID+"/sign-in", nil, bridge); resp.StatusCode != http.StatusOK {
		t.Fatalf("sign-in: %d %s", resp.StatusCode, data)
	}
	opened, err := url.Parse(h.shell.lastOpened())
	if err != nil || !strings.HasPrefix(opened.String(), hub.URL+"/?") {
		t.Fatalf("expected the hub to open in the browser, got %q", h.shell.lastOpened())
	}
	challenge, state := opened.Query().Get("client_auth"), opened.Query().Get("client_state")
	if !handoff.ValidChallenge(challenge) || !handoff.ValidState(state) {
		t.Fatalf("browser URL must carry a valid challenge and state: %q", opened)
	}
	if !h.core.Snapshot().SignIn.Pending {
		t.Fatalf("state should show a pending sign-in")
	}
	hub.mu.Lock()
	hub.challenge = challenge
	hub.mu.Unlock()

	if err := h.core.HandleDeepLink("ourchive://auth?code=good-code&state=forged-state-000000"); err == nil {
		t.Fatalf("a link with an unknown state must be rejected")
	}
	if err := h.core.HandleDeepLink("ourchive://auth?code=good-code&state=" + state); err != nil {
		t.Fatalf("HandleDeepLink: %v", err)
	}
	if tok, ok := h.core.hubs.Session(saved.ID); !ok || tok != "sid-browser" {
		t.Fatalf("expected browser session stored, got %q", tok)
	}
	if st := h.core.Snapshot().SignIn; st.Pending || st.Error != "" {
		t.Fatalf("sign-in should be complete, got %+v", st)
	}
	if err := h.core.HandleDeepLink("ourchive://auth?code=good-code&state=" + state); err == nil {
		t.Fatalf("a sign-in link must only work once")
	}
	for _, bad := range []string{"https://evil.example/auth", "ourchive://launch-missiles", "ourchive://enroll?hub=x&token=y"} {
		if err := h.core.HandleDeepLink(bad); err == nil {
			t.Fatalf("HandleDeepLink(%q) should fail", bad)
		}
	}
}

func TestOpenLinkOnlyShowsTheWindow(t *testing.T) {
	h := newHarness(t, false)
	before := len(h.core.Snapshot().Hubs)
	if err := h.core.HandleDeepLink("ourchive://open?hub=https://evil.example&token=x"); err != nil {
		t.Fatalf("HandleDeepLink: %v", err)
	}
	h.shell.mu.Lock()
	shown := h.shell.shown
	h.shell.mu.Unlock()
	if shown != 1 {
		t.Fatalf("window shown %d times, want 1", shown)
	}
	if got := len(h.core.Snapshot().Hubs); got != before {
		t.Fatalf("an open link must not add hubs: %d -> %d", before, got)
	}
	if h.core.Snapshot().PendingEnrollment != nil {
		t.Fatal("an open link must not queue an enrollment")
	}
}

func TestEnrollmentLinkNeedsConfirmation(t *testing.T) {
	hub := newFakeHub(t, APIVersion)
	h := newHarness(t, true)

	if err := h.core.HandleDeepLink("ourchive://enroll?hub=" + url.QueryEscape(hub.URL) + "&token=tok-1"); err != nil {
		t.Fatalf("HandleDeepLink: %v", err)
	}
	st := h.core.Snapshot()
	if st.PendingEnrollment == nil || st.PendingEnrollment.HubURL != hub.URL || st.PendingEnrollment.HubSaved {
		t.Fatalf("expected a pending enrollment for an unsaved hub, got %+v", st.PendingEnrollment)
	}
	if len(h.enroll) != 0 {
		t.Fatalf("a deep link must never enroll without confirmation")
	}

	// Dismissing drops it.
	if resp, _ := h.do(t, http.MethodPost, "/_client/agent/enrollment", map[string]any{"accept": false}, bridge); resp.StatusCode != http.StatusOK {
		t.Fatalf("dismiss failed")
	}
	if h.core.Snapshot().PendingEnrollment != nil || len(h.enroll) != 0 {
		t.Fatalf("dismissed enrollment must not run")
	}

	_ = h.core.HandleDeepLink("ourchive://enroll?hub=" + url.QueryEscape(hub.URL) + "&token=tok-2")
	resp, data := h.do(t, http.MethodPost, "/_client/agent/enrollment", map[string]any{"accept": true, "name": "desk", "read_only": true}, bridge)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm: %d %s", resp.StatusCode, data)
	}
	if len(h.enroll) != 1 || h.enroll[0].HubURL != hub.URL || h.enroll[0].Token != "tok-2" || !h.enroll[0].ReadOnly || h.enroll[0].Name != "desk" {
		t.Fatalf("unexpected enroll request %+v", h.enroll)
	}
	if len(h.agent.added) != 1 || h.agent.added[0] != "/tmp/hubs/tok-2.json" {
		t.Fatalf("enrolled hub must be handed to the agent, got %v", h.agent.added)
	}
	if _, ok := h.core.hubs.FindByURL(hub.URL); !ok {
		t.Fatalf("confirming should also save the hub")
	}

	// Direct enrollment always targets the active hub; the page cannot name another.
	resp, _ = h.do(t, http.MethodPost, "/_client/agent/enroll", map[string]any{"token": "tok-3", "hub_url": "https://evil.example"}, bridge)
	if resp.StatusCode != http.StatusOK || h.enroll[1].HubURL != hub.URL {
		t.Fatalf("direct enroll must use the active hub, got %+v", h.enroll)
	}

	h.agent.board.Update(hub.URL, func(s *agentstatus.HubStatus) { s.State = agentstatus.StateFailed })
	if resp, _ := h.do(t, http.MethodPost, "/_client/agent/retry", map[string]string{"hub_url": ""}, bridge); resp.StatusCode != http.StatusOK || len(h.agent.retry) != 1 {
		t.Fatalf("retry-all should retry each agent hub, got %v", h.agent.retry)
	}
	if h.core.Snapshot().AgentSummary != agentstatus.SummaryFailed {
		t.Fatalf("agent summary should be relayed into client state")
	}
}

func TestContributorUnsupportedBuild(t *testing.T) {
	h := newHarness(t, false)
	if !errors.Is(h.core.HandleDeepLink("ourchive://enroll?hub=https://x.example&token=t"), ErrContributorUnsupported) {
		t.Fatalf("expected ErrContributorUnsupported")
	}
	if h.core.Snapshot().Platform.Contributor {
		t.Fatalf("platform must not advertise contributing without an agent")
	}
}

func TestDownloadSavesThroughNativeDialog(t *testing.T) {
	hub := newFakeHub(t, APIVersion)
	h := newHarness(t, false)
	h.addHub(t, hub.URL)
	h.do(t, http.MethodPost, "/api/auth/login", map[string]string{}, nil)

	if _, err := h.core.StartDownload("https://evil.example/x", "x"); err == nil {
		t.Fatalf("absolute URLs must be refused")
	}
	d, err := h.core.StartDownload("/api/vfs/file", "../../notes.txt")
	if err != nil || d == nil {
		t.Fatalf("StartDownload: %v", err)
	}
	want := filepath.Join(h.shell.saveTo, "notes.txt")
	if d.Path != want {
		t.Fatalf("suggested name must be reduced to a base name: %q", d.Path)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		st := h.core.Snapshot().Downloads
		if len(st) == 1 && st[0].State != "running" {
			if st[0].State != "done" || st[0].Bytes != 11 {
				t.Fatalf("download ended badly: %+v", st[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("download did not finish: %+v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if data, _ := os.ReadFile(want); string(data) != "hello world" {
		t.Fatalf("unexpected file contents %q", data)
	}
	if matches, _ := filepath.Glob(filepath.Join(h.shell.saveTo, "*.part")); len(matches) != 0 {
		t.Fatalf("temporary files left behind: %v", matches)
	}

	h.shell.cancelDl = true
	if d, err := h.core.StartDownload("/api/vfs/file", "x"); d != nil || err != nil {
		t.Fatalf("cancelled dialog should be a no-op, got %+v %v", d, err)
	}
}

func TestHubStorePersistsPrivately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client", "state.json")
	s, err := OpenHubStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.Add("", "Archive.Example.com")
	b, _ := s.Add("Lab", "http://localhost:8080")
	if a.URL != "https://archive.example.com" || b.ID != "http-localhost-8080" {
		t.Fatalf("unexpected normalization: %+v %+v", a, b)
	}
	if dup, _ := s.Add("", "https://archive.example.com/"); dup.ID != a.ID || len(s.List()) != 2 {
		t.Fatalf("adding the same address twice must not duplicate it")
	}
	if _, err := s.Add("", "ftp://nope"); err == nil {
		t.Fatalf("non-http schemes must be rejected")
	}
	_ = s.SetSession(b.ID, "tok", time.Now().Add(time.Hour))
	_ = s.SetActive(b.ID)

	reopened, err := OpenHubStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if act, _ := reopened.Active(); act.ID != b.ID {
		t.Fatalf("active hub not persisted")
	}
	if tok, ok := reopened.Session(b.ID); !ok || tok != "tok" {
		t.Fatalf("session not persisted")
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("state file holds session tokens and must be 0600, got %v", info.Mode().Perm())
		}
	}
	_ = reopened.Remove(b.ID)
	if act, _ := reopened.Active(); act.ID != a.ID {
		t.Fatalf("removing the active hub should fall back to another")
	}
	if _, ok := reopened.Session(b.ID); ok {
		t.Fatalf("removing a hub must drop its session")
	}
}

func TestStateLongPoll(t *testing.T) {
	hub := newFakeHub(t, APIVersion+1)
	h := newHarness(t, false)

	_, data := h.do(t, http.MethodGet, "/_client/state", nil, nil)
	var first State
	_ = json.Unmarshal(data, &first)
	if len(first.Hubs) != 0 {
		t.Fatalf("expected empty initial state")
	}

	// A long-poll with the current version waits until something changes.
	poll := func(since uint64) (State, error) {
		resp, err := http.Get(fmt.Sprintf("%s/_client/state?since=%d", h.app.URL, since))
		if err != nil {
			return State{}, err
		}
		defer resp.Body.Close()
		var st State
		return st, json.NewDecoder(resp.Body).Decode(&st)
	}
	type result struct {
		st  State
		err error
	}
	done := make(chan result, 1)
	go func() {
		st, err := poll(first.Version)
		done <- result{st, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("long-poll returned before any change: %+v %v", r.st, r.err)
	case <-time.After(100 * time.Millisecond):
	}
	h.addHub(t, hub.URL)
	var woke State
	select {
	case r := <-done:
		if r.err != nil || r.st.Version <= first.Version {
			t.Fatalf("long-poll should return a newer version, got %+v (%v)", r.st, r.err)
		}
		woke = r.st
	case <-time.After(3 * time.Second):
		t.Fatal("long-poll did not wake on change")
	}
	// Following versions the way the UI does converges on the saved hub.
	for len(woke.Hubs) == 0 {
		next, err := poll(woke.Version)
		if err != nil {
			t.Fatal(err)
		}
		woke = next
	}

	// The newer-API hub is flagged once its health check lands.
	deadline := time.Now().Add(3 * time.Second)
	for {
		st := h.core.Snapshot()
		if info := st.Hubs[0].Info; info != nil {
			if info.Compatible || !strings.Contains(info.Error, "update the app") {
				t.Fatalf("a newer hub API must be flagged, got %+v", info)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hub info never arrived")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if resp, _ := h.do(t, http.MethodGet, "/_client/state?since=abc", nil, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad since must be rejected, got %d", resp.StatusCode)
	}
}

// bufferedWriter hides http.Flusher, like Wails' WebView2 response writer, which delivers a
// response only after the handler returns.
type bufferedWriter struct{ http.ResponseWriter }

func TestEventStreamsRefusedWhereWebviewCannotStream(t *testing.T) {
	hub := newFakeHub(t, APIVersion)
	h := newHarness(t, false)
	h.addHub(t, hub.URL)

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() {
		h.core.Middleware(http.NotFoundHandler()).ServeHTTP(bufferedWriter{rec}, req)
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("an event stream through a non-streaming writer must not hang")
	}
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501 so EventSource gives up and the UI polls, got %d", rec.Code)
	}

	// Ordinary API calls through the same kind of writer still work.
	req = httptest.NewRequest(http.MethodGet, "/api/echo", nil)
	rec = httptest.NewRecorder()
	h.core.Middleware(http.NotFoundHandler()).ServeHTTP(bufferedWriter{rec}, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("plain API call through a buffered writer failed: %d", rec.Code)
	}
}

func TestOpenRangesAreChunkedWhereWebviewBuffers(t *testing.T) {
	hub := newFakeHub(t, APIVersion)
	h := newHarness(t, false)
	h.addHub(t, hub.URL)

	get := func(w http.ResponseWriter, rng string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/vfs/video", nil)
		req.Header.Set("Range", rng)
		rec := w.(*httptest.ResponseRecorder)
		h.core.Middleware(http.NotFoundHandler()).ServeHTTP(w, req)
		return rec
	}

	// Buffered (WebView2-like): "bytes=0-" of a 20 MiB file comes back as one 8 MiB chunk.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/vfs/video", nil)
	req.Header.Set("Range", "bytes=0-")
	h.core.Middleware(http.NotFoundHandler()).ServeHTTP(bufferedWriter{rec}, req)
	if rec.Code != http.StatusPartialContent || rec.Body.Len() != bufferedRangeCap {
		t.Fatalf("expected an 8 MiB 206 chunk, got %d with %d bytes", rec.Code, rec.Body.Len())
	}
	if cr := rec.Header().Get("Content-Range"); cr != fmt.Sprintf("bytes 0-%d/%d", bufferedRangeCap-1, 20<<20) {
		t.Fatalf("unexpected Content-Range %q", cr)
	}

	// Streaming webviews pass the range through untouched.
	rec = get(httptest.NewRecorder(), "bytes=0-")
	if rec.Body.Len() != 20<<20 {
		t.Fatalf("streaming writer should get the whole open range, got %d bytes", rec.Body.Len())
	}

	// Explicit and multi-part ranges are never rewritten.
	for _, spec := range []string{"bytes=0-99", "bytes=-500", "bytes=0-1,5-9", "items=0-"} {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.Header.Set("Range", spec)
		capOpenRange(r, bufferedRangeCap)
		if got := r.Header.Get("Range"); got != spec {
			t.Fatalf("capOpenRange(%q) rewrote it to %q", spec, got)
		}
	}
}

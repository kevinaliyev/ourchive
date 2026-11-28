// Package clientcore is the desktop client's native backend, independent of any GUI toolkit.
//
// The client's window shows the same React UI the hub serves, bundled into the app. Core sits in
// front of those assets as an http.Handler and gives the bundled UI what being served by a hub
// used to give it for free:
//
//   - /api/* and /downloads/* are reverse-proxied to the active hub, with that hub's session
//     injected from the HubStore, so the UI's relative fetch("/api/…") calls work unchanged and
//     the webview never holds hub cookies.
//   - /_client/* is a small JSON bridge for things only the app can do: switch hubs, sign in
//     through the system browser, save downloads with a native dialog, and drive the embedded
//     contributor agent. The UI follows state changes by long-polling /_client/state.
//
// The GUI shell (cmd/client, Wails today) supplies a Shell for native dialogs, links and
// notifications, and forwards ourchive:// deep links to HandleDeepLink.
package clientcore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ourchive/ourchive/internal/agentstatus"
)

// APIVersion is the hub API version this client understands (see /api/health api_version).
const APIVersion = 1

// Shell is implemented by the native host.
type Shell interface {
	// OpenExternal opens an http(s) URL in the user's default browser.
	OpenExternal(url string) error
	// SaveFileDialog asks where to save a file. An empty path and nil error mean "cancelled".
	SaveFileDialog(suggestedName string) (string, error)
	// Notify shows a desktop notification. Failures are ignored.
	Notify(title, body string)
	// ShowWindow brings the main window to the front.
	ShowWindow()
}

// Agent is the part of the contributor agent the client drives. It is satisfied by
// *agentcore.Runner and is nil on platforms where contributing is unsupported.
type Agent interface {
	Status() []agentstatus.HubStatus
	Subscribe() (<-chan []agentstatus.HubStatus, func())
	Retry(hubURL string) bool
	AddHub(configPath string) error
}

// EnrollRequest is what the client needs to enroll this machine with a hub.
type EnrollRequest struct {
	HubURL   string
	Token    string
	Name     string
	ReadOnly bool
}

// EnrollFunc enrolls this machine and returns the written agent config path.
type EnrollFunc func(ctx context.Context, req EnrollRequest) (configPath string, err error)

// PlatformInfo tells the UI what this build can do.
type PlatformInfo struct {
	OS          string `json:"os"`
	Mobile      bool   `json:"mobile"`
	Contributor bool   `json:"contributor"`
	// ContributorNote explains why contributing is unavailable, when it is.
	ContributorNote string `json:"contributor_note,omitempty"`
}

// Options configures New.
type Options struct {
	// StatePath is where saved hubs and sessions are persisted. Empty keeps them in memory.
	StatePath string
	// Version is the client's version string, shown in the UI.
	Version string
	Shell   Shell
	// Agent and Enroll are nil when this build cannot contribute storage (e.g. iOS).
	Agent  Agent
	Enroll EnrollFunc
	// Mobile marks iOS/Android builds.
	Mobile bool
	// ContributorNote is shown when Agent is nil, e.g. "another agent is already running".
	ContributorNote string
	// HTTPClient is used for direct calls to hubs (health, sign-in exchange, downloads).
	HTTPClient *http.Client
	// Transport is used by the API proxy. Nil means a clone of http.DefaultTransport.
	Transport http.RoundTripper
}

// Core is the client backend. Create it with New and mount Handler in front of the UI assets.
type Core struct {
	opts  Options
	hubs  *HubStore
	shell Shell
	http  *http.Client
	proxy http.Handler

	mu          sync.Mutex
	hubInfo     map[string]HubInfo
	signIns     map[string]pendingSignIn
	signInState *SignInStatus
	enrollment  *pendingEnrollment
	downloads   []*Download
	nextDLID    int

	subsMu  sync.Mutex
	subs    map[int]chan struct{}
	nextID  int
	version atomic.Uint64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// HubInfo is what the client last learned from a hub's /api/health.
type HubInfo struct {
	Reachable  bool      `json:"reachable"`
	Name       string    `json:"name,omitempty"`
	Version    string    `json:"version,omitempty"`
	APIVersion int       `json:"api_version"`
	Compatible bool      `json:"compatible"`
	Error      string    `json:"error,omitempty"`
	CheckedAt  time.Time `json:"checked_at"`
}

// HubView is a saved hub as the UI sees it.
type HubView struct {
	Hub
	Active   bool     `json:"active"`
	SignedIn bool     `json:"signed_in"`
	Info     *HubInfo `json:"info,omitempty"`
}

// SignInStatus reports the outcome of the most recent browser sign-in.
type SignInStatus struct {
	HubID   string    `json:"hub_id"`
	Pending bool      `json:"pending"`
	Error   string    `json:"error,omitempty"`
	At      time.Time `json:"at"`
}

// PendingEnrollmentView is an ourchive://enroll link waiting for the user to confirm.
type PendingEnrollmentView struct {
	HubURL     string    `json:"hub_url"`
	HubSaved   bool      `json:"hub_saved"`
	ReceivedAt time.Time `json:"received_at"`
}

// State is everything the UI and tray render.
type State struct {
	// Version increases on every change; pass it back as ?since= to wait for the next one.
	Version           uint64                  `json:"version"`
	ClientVersion     string                  `json:"client_version"`
	APIVersion        int                     `json:"api_version"`
	Platform          PlatformInfo            `json:"platform"`
	Hubs              []HubView               `json:"hubs"`
	ActiveHubID       string                  `json:"active_hub_id,omitempty"`
	Agent             []agentstatus.HubStatus `json:"agent"`
	AgentSummary      agentstatus.Summary     `json:"agent_summary"`
	SignIn            *SignInStatus           `json:"sign_in,omitempty"`
	PendingEnrollment *PendingEnrollmentView  `json:"pending_enrollment,omitempty"`
	Downloads         []Download              `json:"downloads"`
}

// New opens the hub store and starts background work (agent status relay, hub health checks).
func New(opts Options) (*Core, error) {
	if opts.Shell == nil {
		return nil, errors.New("clientcore: Shell is required")
	}
	hubs, err := OpenHubStore(opts.StatePath)
	if err != nil {
		return nil, err
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Core{
		opts:    opts,
		hubs:    hubs,
		shell:   opts.Shell,
		http:    httpClient,
		hubInfo: map[string]HubInfo{},
		signIns: map[string]pendingSignIn{},
		subs:    map[int]chan struct{}{},
		ctx:     ctx,
		cancel:  cancel,
	}
	c.proxy = c.newProxy()

	if opts.Agent != nil {
		updates, unsubscribe := opts.Agent.Subscribe()
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			defer unsubscribe()
			for {
				select {
				case <-ctx.Done():
					return
				case _, ok := <-updates:
					if !ok {
						return
					}
					c.changed()
				}
			}
		}()
	}
	if h, ok := hubs.Active(); ok {
		c.probeHubAsync(h)
	}
	return c, nil
}

// Close stops background work. In-flight downloads are cancelled.
func (c *Core) Close() {
	c.cancel()
	c.wg.Wait()
}

// Hubs exposes the hub store (for the shell's tray menu).
func (c *Core) Hubs() *HubStore { return c.hubs }

// Handler serves the bridge and the API proxy, and passes everything else to assets.
func (c *Core) Handler(assets http.Handler) http.Handler { return c.Middleware(assets) }

// Middleware is Handler in the shape GUI asset servers expect.
func (c *Core) Middleware(next http.Handler) http.Handler {
	bridge := c.bridgeMux()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/_client/"):
			if !sameOriginRequest(r) {
				writeJSONError(w, http.StatusForbidden, "cross-origin request blocked")
				return
			}
			bridge.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/downloads/"):
			if !sameOriginRequest(r) {
				writeJSONError(w, http.StatusForbidden, "cross-origin request blocked")
				return
			}
			if wantsEventStream(r) && !canStream(w) {
				// Some webviews (WebView2 under Wails) deliver a response only once the handler
				// returns, so an endless event stream would just pile up in memory. Refuse it; the
				// UI already falls back to polling when its EventSource fails.
				writeJSONError(w, http.StatusNotImplemented, "live event streams are not supported in this webview")
				return
			}
			if !canStream(w) {
				capOpenRange(r, bufferedRangeCap)
			}
			restoreBodyLength(r)
			c.proxy.ServeHTTP(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// sameOriginRequest rejects requests a browser engine marks as coming from another origin. Pages
// the app shows are all same-origin; anything else (for example an HTML file opened from a hub,
// which the hub serves with a sandbox CSP and so runs in an opaque origin) must not be able to use
// the injected hub session or the bridge. Requests without fetch metadata are allowed: not every
// webview sends it for custom schemes.
func sameOriginRequest(r *http.Request) bool {
	switch strings.ToLower(r.Header.Get("Sec-Fetch-Site")) {
	case "", "same-origin", "none":
		return true
	default:
		return false
	}
}

// restoreBodyLength undoes a quirk of WebView2: it gives the app request bodies without a
// Content-Length, so the request arrives with ContentLength 0 and a body attached. The reverse proxy
// reads 0 as "no body" and would drop it (the hub then answers "invalid JSON body"). A body that
// turns out to have data is marked as unknown-length so it is forwarded chunked, without buffering
// uploads; one that is really empty becomes http.NoBody.
func restoreBodyLength(r *http.Request) {
	if r.ContentLength != 0 || r.Body == nil || r.Body == http.NoBody {
		return
	}
	var first [1]byte
	n, err := io.ReadFull(r.Body, first[:])
	if n == 0 {
		if err == io.EOF {
			_ = r.Body.Close()
			r.Body = http.NoBody
		}
		return
	}
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(first[:n]), r.Body), r.Body}
	r.ContentLength = -1
	r.Header.Del("Content-Length")
}

// bufferedRangeCap is the largest byte range requested from a hub on behalf of a webview that
// buffers whole responses. Media elements ask for "bytes=N-" and happily continue with the next
// range after a 206, so capping keeps a video preview from being pulled into memory in one piece.
const bufferedRangeCap = 8 << 20

// capOpenRange rewrites an open-ended single range ("bytes=N-") to at most max bytes.
func capOpenRange(r *http.Request, max int64) {
	spec := strings.TrimSpace(r.Header.Get("Range"))
	if !strings.HasPrefix(spec, "bytes=") || !strings.HasSuffix(spec, "-") || strings.Contains(spec, ",") {
		return
	}
	start, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(spec, "bytes="), "-"), 10, 64)
	if err != nil || start < 0 {
		return
	}
	r.Header.Set("Range", "bytes="+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(start+max-1, 10))
}

func wantsEventStream(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

// canStream reports whether bytes written to w reach the page before the handler returns.
func canStream(w http.ResponseWriter) bool {
	_, ok := w.(http.Flusher)
	return ok
}

// WaitForChange blocks until the state version exceeds since, ctx ends, or timeout passes, and
// returns the current State either way. It is how the UI follows state on every platform,
// including webviews that cannot stream responses.
func (c *Core) WaitForChange(ctx context.Context, since uint64, timeout time.Duration) State {
	updates, unsubscribe := c.Subscribe()
	defer unsubscribe()
	if c.version.Load() > since {
		return c.Snapshot()
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return c.Snapshot()
		case <-c.ctx.Done():
			return c.Snapshot()
		case <-timer.C:
			return c.Snapshot()
		case <-updates:
			if c.version.Load() > since {
				return c.Snapshot()
			}
		}
	}
}

// Snapshot returns the current State.
func (c *Core) Snapshot() State {
	active, _ := c.hubs.Active()
	st := State{
		Version:       c.version.Load(),
		ClientVersion: c.opts.Version,
		APIVersion:    APIVersion,
		Platform: PlatformInfo{
			OS:              runtime.GOOS,
			Mobile:          c.opts.Mobile,
			Contributor:     c.opts.Agent != nil && c.opts.Enroll != nil,
			ContributorNote: c.opts.ContributorNote,
		},
		ActiveHubID: active.ID,
		Agent:       []agentstatus.HubStatus{},
		Downloads:   []Download{},
	}
	for _, h := range c.hubs.List() {
		_, signedIn := c.hubs.Session(h.ID)
		v := HubView{Hub: h, Active: h.ID == active.ID, SignedIn: signedIn}
		c.mu.Lock()
		if info, ok := c.hubInfo[h.ID]; ok {
			info := info
			v.Info = &info
		}
		c.mu.Unlock()
		st.Hubs = append(st.Hubs, v)
	}
	if st.Hubs == nil {
		st.Hubs = []HubView{}
	}
	if c.opts.Agent != nil {
		st.Agent = c.opts.Agent.Status()
	}
	st.AgentSummary = agentstatus.Summarize(st.Agent)

	c.mu.Lock()
	if c.signInState != nil {
		s := *c.signInState
		st.SignIn = &s
	}
	if c.enrollment != nil && !c.enrollment.expired(time.Now()) {
		_, saved := c.hubs.FindByURL(c.enrollment.HubURL)
		st.PendingEnrollment = &PendingEnrollmentView{HubURL: c.enrollment.HubURL, HubSaved: saved, ReceivedAt: c.enrollment.ReceivedAt}
	}
	for _, d := range c.downloads {
		st.Downloads = append(st.Downloads, *d)
	}
	c.mu.Unlock()
	return st
}

// Subscribe returns a channel that receives a signal after every state change (coalesced), plus
// an unsubscribe function. Call Snapshot to read the new state.
func (c *Core) Subscribe() (<-chan struct{}, func()) {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	id := c.nextID
	c.nextID++
	ch := make(chan struct{}, 1)
	c.subs[id] = ch
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			c.subsMu.Lock()
			defer c.subsMu.Unlock()
			delete(c.subs, id)
			close(ch)
		})
	}
}

func (c *Core) changed() {
	c.version.Add(1)
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	for _, ch := range c.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

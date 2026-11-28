package clientcore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ourchive/ourchive/internal/handoff"
)

// DeepLinkScheme is the URL scheme the app registers with the OS.
const DeepLinkScheme = "ourchive"

const (
	signInTTL     = 10 * time.Minute
	maxSignIns    = 8
	enrollmentTTL = 15 * time.Minute
)

type pendingSignIn struct {
	hubID    string
	verifier string
	created  time.Time
}

type pendingEnrollment struct {
	HubURL     string
	Token      string
	Name       string
	ReceivedAt time.Time
}

func (p *pendingEnrollment) expired(now time.Time) bool {
	return now.Sub(p.ReceivedAt) > enrollmentTTL
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// BeginBrowserSignIn opens the hub's sign-in page in the system browser. The user can use any
// method the hub supports there (password, passkey, SSO); the hub then sends an ourchive://auth
// link back to HandleDeepLink.
func (c *Core) BeginBrowserSignIn(hubID string) error {
	hub, ok := c.hubs.Get(hubID)
	if !ok {
		return ErrUnknownHub
	}
	verifier, err := randomToken(32) // 43 chars, RFC 7636 compliant
	if err != nil {
		return err
	}
	state, err := randomToken(24)
	if err != nil {
		return err
	}

	now := time.Now()
	c.mu.Lock()
	for s, p := range c.signIns {
		if now.Sub(p.created) > signInTTL {
			delete(c.signIns, s)
		}
	}
	if len(c.signIns) >= maxSignIns {
		// Drop the oldest attempt; users retrying sign-in should never be blocked.
		var oldest string
		for s, p := range c.signIns {
			if oldest == "" || p.created.Before(c.signIns[oldest].created) {
				oldest = s
			}
		}
		delete(c.signIns, oldest)
	}
	c.signIns[state] = pendingSignIn{hubID: hub.ID, verifier: verifier, created: now}
	c.signInState = &SignInStatus{HubID: hub.ID, Pending: true, At: now.UTC()}
	c.mu.Unlock()
	c.changed()

	q := url.Values{}
	q.Set("client_auth", handoff.Challenge(verifier))
	q.Set("client_state", state)
	return c.shell.OpenExternal(hub.URL + "/?" + q.Encode())
}

// HandleDeepLink processes an ourchive:// URL the OS handed to the app:
//
//	ourchive://auth?code=…&state=…        completes a browser sign-in
//	ourchive://enroll?hub=…&token=…       asks the user to confirm contributing this machine
//
// Anything else is rejected. Deep links come from outside the app (any web page can trigger one),
// so neither kind acts without something only this app holds (the PKCE verifier) or an explicit
// confirmation in the app's own UI.
func (c *Core) HandleDeepLink(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(u.Scheme, DeepLinkScheme) {
		return errors.New("not an ourchive:// link")
	}
	// ourchive://auth?… parses with Host "auth"; tolerate ourchive:auth and ourchive:///auth too.
	action := strings.ToLower(strings.Trim(u.Host+u.Opaque+u.Path, "/"))
	if i := strings.IndexByte(action, '?'); i >= 0 {
		action = action[:i]
	}
	q := u.Query()
	switch action {
	case "auth":
		return c.completeSignIn(q.Get("state"), q.Get("code"))
	case "enroll":
		return c.queueEnrollment(q.Get("hub"), q.Get("token"), q.Get("name"))
	case "open":
		// The website's "Open app" button. It only brings the window forward: it carries no
		// parameters and never adds a hub, signs in or changes anything.
		c.shell.ShowWindow()
		return nil
	default:
		return fmt.Errorf("unsupported ourchive:// action %q", action)
	}
}

func (c *Core) completeSignIn(state, code string) error {
	c.mu.Lock()
	p, ok := c.signIns[state]
	if ok {
		delete(c.signIns, state)
	}
	c.mu.Unlock()
	if !ok || time.Since(p.created) > signInTTL {
		return errors.New("this sign-in link is not from a sign-in started in this app, or it has expired")
	}
	hub, ok := c.hubs.Get(p.hubID)
	if !ok {
		return ErrUnknownHub
	}

	err := c.exchangeCode(hub, code, p.verifier)
	c.mu.Lock()
	c.signInState = &SignInStatus{HubID: hub.ID, At: time.Now().UTC()}
	if err != nil {
		c.signInState.Error = err.Error()
	}
	c.mu.Unlock()
	if err != nil {
		c.changed()
		c.shell.Notify("Sign-in failed", err.Error())
		return err
	}
	_ = c.hubs.SetActive(hub.ID)
	c.probeHubAsync(hub)
	c.changed()
	c.shell.ShowWindow()
	c.shell.Notify("Signed in", "You're signed in to "+hub.Name+".")
	return nil
}

func (c *Core) exchangeCode(hub Hub, code, verifier string) error {
	body, _ := json.Marshal(map[string]string{"code": code, "verifier": verifier})
	ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hub.URL+"/api/auth/client/exchange", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach %s: %w", hub.Name, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("%s rejected the sign-in: %s", hub.Name, e.Error)
	}
	var res struct {
		SessionCookie string    `json:"session_cookie"`
		SessionToken  string    `json:"session_token"`
		ExpiresAt     time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(data, &res); err != nil || res.SessionToken == "" {
		return errors.New("unexpected sign-in response from hub")
	}
	if res.SessionCookie != "" && res.SessionCookie != sessionCookieName {
		return fmt.Errorf("hub uses session cookie %q, which this app does not support", res.SessionCookie)
	}
	return c.hubs.SetSession(hub.ID, res.SessionToken, res.ExpiresAt)
}

// probeHubAsync refreshes HubInfo for hub in the background.
func (c *Core) probeHubAsync(hub Hub) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.probeHub(hub)
	}()
}

func (c *Core) probeHub(hub Hub) HubInfo {
	ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
	defer cancel()
	info := HubInfo{CheckedAt: time.Now().UTC()}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, hub.URL+"/api/health", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		info.Error = err.Error()
	} else {
		var h struct {
			Status     string `json:"status"`
			Version    string `json:"version"`
			HubName    string `json:"hub_name"`
			APIVersion int    `json:"api_version"`
		}
		decErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&h)
		resp.Body.Close()
		switch {
		case resp.StatusCode != http.StatusOK || decErr != nil || h.Status != "ok":
			info.Error = "this address does not look like an Ourchive hub"
		default:
			info.Reachable = true
			info.Name = h.HubName
			info.Version = h.Version
			info.APIVersion = h.APIVersion
			info.Compatible = h.APIVersion == APIVersion
			switch {
			case h.APIVersion < APIVersion:
				info.Error = "this hub runs an older version; ask its admin to update it"
			case h.APIVersion > APIVersion:
				info.Error = "this hub is newer than this app; update the app"
			}
		}
	}
	c.mu.Lock()
	c.hubInfo[hub.ID] = info
	c.mu.Unlock()
	if info.Reachable && info.Name != "" {
		if cur, ok := c.hubs.Get(hub.ID); ok && cur.Name == defaultHubName(cur.URL) {
			_ = c.hubs.Rename(hub.ID, info.Name)
		}
	}
	c.changed()
	return info
}

func (c *Core) recordUnreachable(hub Hub, err error) {
	if hub.ID == "" {
		return
	}
	c.mu.Lock()
	info := c.hubInfo[hub.ID]
	wasReachable := info.Reachable || info.CheckedAt.IsZero()
	info.Reachable = false
	info.Error = err.Error()
	info.CheckedAt = time.Now().UTC()
	c.hubInfo[hub.ID] = info
	c.mu.Unlock()
	if wasReachable {
		c.changed()
	}
}

func defaultHubName(hubURL string) string {
	u, err := url.Parse(hubURL)
	if err != nil {
		return hubURL
	}
	return u.Host
}

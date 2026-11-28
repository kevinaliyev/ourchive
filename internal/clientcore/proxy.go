package clientcore

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// sessionCookieName must match auth.SessionCookieName on the hub.
const sessionCookieName = "ourchive_session"

type proxyHubKey struct{}

// newProxy builds the reverse proxy that forwards the bundled UI's /api and /downloads calls to
// the active hub. It plays the role of the browser's cookie jar: the hub session lives in the
// HubStore, is attached to each outbound request, and Set-Cookie responses update the store
// instead of reaching the webview.
func (c *Core) newProxy() http.Handler {
	transport := c.opts.Transport
	if transport == nil {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	}
	rp := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1, // stream SSE (/api/events) and large downloads immediately
		Rewrite: func(pr *httputil.ProxyRequest) {
			hub := pr.In.Context().Value(proxyHubKey{}).(Hub)
			target, _ := url.Parse(hub.URL)
			pr.SetURL(target)
			pr.Out.Host = target.Host

			// The webview's own cookies, origin and fetch metadata describe the app, not the hub.
			// Origin checks are enforced by Middleware before a request gets here.
			for _, h := range []string{"Cookie", "Origin", "Referer", "Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest", "Sec-Fetch-User"} {
				pr.Out.Header.Del(h)
			}
			if token, ok := c.hubs.Session(hub.ID); ok {
				pr.Out.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
			}
			pr.Out.Header.Set("X-Ourchive-Client", c.opts.Version)
		},
		ModifyResponse: func(resp *http.Response) error {
			hub, _ := resp.Request.Context().Value(proxyHubKey{}).(Hub)
			c.captureSession(hub, resp)
			resp.Header.Del("Set-Cookie")
			rewriteLocation(hub, resp)
			// Defence in depth: anything the hub returns without its own CSP must not be able to
			// run script in the app's origin if it is ever rendered as a document.
			if resp.Header.Get("Content-Security-Policy") == "" {
				resp.Header.Set("Content-Security-Policy", "default-src 'none'; sandbox")
			}
			resp.Header.Set("X-Content-Type-Options", "nosniff")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			hub, _ := r.Context().Value(proxyHubKey{}).(Hub)
			c.recordUnreachable(hub, err)
			writeJSONError(w, http.StatusBadGateway, "could not reach "+hub.Name+": "+err.Error())
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub, ok := c.hubs.Active()
		if !ok {
			writeJSONError(w, http.StatusServiceUnavailable, "no hub selected")
			return
		}
		rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), proxyHubKey{}, hub)))
	})
}

// captureSession records the hub's session cookie (login) or its removal (logout).
func (c *Core) captureSession(hub Hub, resp *http.Response) {
	if hub.ID == "" {
		return
	}
	for _, ck := range resp.Cookies() {
		if ck.Name != sessionCookieName {
			continue
		}
		expired := ck.Value == "" || ck.MaxAge < 0 || (!ck.Expires.IsZero() && ck.Expires.Before(time.Now()))
		if expired {
			_ = c.hubs.ClearSession(hub.ID)
		} else {
			exp := ck.Expires
			if ck.MaxAge > 0 {
				exp = time.Now().Add(time.Duration(ck.MaxAge) * time.Second)
			}
			_ = c.hubs.SetSession(hub.ID, ck.Value, exp)
		}
		c.changed()
	}
}

// rewriteLocation keeps hub redirects inside the app: an absolute Location on the hub's origin
// becomes a relative path. Redirects elsewhere (an identity provider) are left alone; the UI opens
// those through the system browser.
func rewriteLocation(hub Hub, resp *http.Response) {
	loc := resp.Header.Get("Location")
	if loc == "" || hub.URL == "" {
		return
	}
	u, err := url.Parse(loc)
	if err != nil || !u.IsAbs() {
		return
	}
	base, _ := url.Parse(hub.URL)
	if strings.EqualFold(u.Scheme, base.Scheme) && strings.EqualFold(u.Host, base.Host) {
		rel := u.EscapedPath()
		if rel == "" {
			rel = "/"
		}
		if u.RawQuery != "" {
			rel += "?" + u.RawQuery
		}
		resp.Header.Set("Location", rel)
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

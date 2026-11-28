package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ourchive/ourchive/internal/audit"
	"github.com/ourchive/ourchive/internal/auth"
	"github.com/ourchive/ourchive/internal/db"
	"github.com/ourchive/ourchive/internal/events"
	"github.com/ourchive/ourchive/internal/handoff"
	"github.com/ourchive/ourchive/internal/settings"
	"github.com/ourchive/ourchive/internal/shares"
	"github.com/ourchive/ourchive/internal/tunnel"
	"github.com/ourchive/ourchive/internal/vfs"
)

type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	// APIVersion lets the desktop client detect a hub it does not understand.
	APIVersion int    `json:"api_version"`
	HubName    string `json:"hub_name"`
	ShareHost  string `json:"share_host"`
	Time       string `json:"time"`
}

// hubApp holds the shared control-plane dependencies used across route modules.
type hubApp struct {
	store            *db.Store
	authSvc          *auth.Service
	broker           *tunnel.Broker
	tagStore         *vfs.TagStore
	settingsStore    *settings.Store
	auditSvc         *audit.Service
	eventBus         *events.Bus
	oidcMgr          *auth.OIDCManager
	passkeyMgr       *auth.PasskeyManager
	authLimiter      *ipRateLimiter
	shareSvc         *shares.Service
	clientHandoff    *handoff.Store
	thumbDir         string
	webDir           string
	distDir          string
	hubName          string
	publicHubURL     string
	agentDownloadURL string
	agentManifestURL string
	shareHost        string
	shareBaseURL     string
}

func main() {
	addr := envOr("OURCHIVE_ADDR", ":8080")
	shareHost := strings.ToLower(envOr("OURCHIVE_SHARE_HOST", "s.localhost"))
	dbPath := envOr("OURCHIVE_DB_PATH", "data/ourchive.db")
	thumbDir := envOr("OURCHIVE_THUMB_DIR", "data/thumbs")
	webDir := envOr("OURCHIVE_WEB_DIR", "web/dist")
	distDir := envOr("OURCHIVE_DIST_DIR", "dist")
	publicHubURL := strings.TrimRight(envOr("OURCHIVE_PUBLIC_URL", "http://localhost"+addr), "/")
	hubName := strings.TrimSpace(os.Getenv("OURCHIVE_HUB_NAME"))
	if hubName == "" {
		if u, err := url.Parse(publicHubURL); err == nil && u.Hostname() != "" {
			hubName = u.Hostname()
		} else {
			hubName = "Ourchive"
		}
	}
	agentDownloadURL := strings.TrimRight(os.Getenv("OURCHIVE_AGENT_DOWNLOAD_URL"), "/")
	agentManifestURL := strings.TrimSpace(os.Getenv("OURCHIVE_AGENT_MANIFEST_URL"))
	_ = os.MkdirAll(thumbDir, 0o755)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := db.OpenSQLite(ctx, dbPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	authSvc := auth.NewService(store)
	broker := tunnel.NewBroker(store)
	tagStore := vfs.NewTagStore(store)
	settingsStore := settings.NewStore(ctx, store)
	auditSvc := audit.NewService(store, settingsStore)
	eventBus := events.NewBus()
	oidcMgr := auth.NewOIDCManager(authSvc, settingsStore)
	passkeyMgr, err := auth.NewPasskeyManager(authSvc, publicHubURL)
	if err != nil {
		log.Printf("webauthn disabled: %v", err)
	}
	authLimiter := newIPRateLimiter(12, 12) // 12 req/min with burst 12 per IP on auth/invite/enroll endpoints

	// Push live node status / traffic heartbeats to SSE subscribers every 2.5s.
	go func() {
		t := time.NewTicker(2500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if eventBus.Count() > 0 {
					eventBus.Publish("nodes.updated", map[string]any{"ts": time.Now().UTC().UnixMilli()})
				}
			}
		}
	}()

	shareBaseURL := envOr("OURCHIVE_SHARE_URL", "http://"+shareHost+addr)
	shareSvc := shares.NewService(store, broker, settingsStore, shareBaseURL, thumbDir)
	shareSvc.SetAudit(auditSvc)

	hub := &hubApp{
		store:            store,
		authSvc:          authSvc,
		broker:           broker,
		tagStore:         tagStore,
		settingsStore:    settingsStore,
		auditSvc:         auditSvc,
		eventBus:         eventBus,
		oidcMgr:          oidcMgr,
		passkeyMgr:       passkeyMgr,
		authLimiter:      authLimiter,
		shareSvc:         shareSvc,
		thumbDir:         thumbDir,
		webDir:           webDir,
		distDir:          distDir,
		hubName:          hubName,
		publicHubURL:     publicHubURL,
		agentDownloadURL: agentDownloadURL,
		agentManifestURL: agentManifestURL,
		shareHost:        shareHost,
		shareBaseURL:     shareBaseURL,
	}

	appMux := http.NewServeMux()
	hub.registerSystemRoutes(appMux)
	hub.registerAuthRoutes(appMux)
	hub.registerNodeRoutes(appMux)
	hub.registerVFSRoutes(appMux)
	hub.registerShareRoutes(appMux)
	hub.registerComputeRoutes(appMux)
	hub.registerClientRoutes(appMux)

	// --- Single-Process Production Web UI Serving (OURCHIVE_WEB_DIR, default: web/dist) ---
	if info, err := os.Stat(filepath.Join(webDir, "index.html")); err == nil && !info.IsDir() {
		log.Printf("serving built Web UI from %s", webDir)
		appMux.HandleFunc("/", newSPAHandler(webDir))
	}

	// --- Isolated Share Subdomain (s.localhost / s.yourdomain.com) ---
	shareHandler := shareSvc.ShareHandler()

	rootHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Baseline security headers on all responses.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")

		host := strings.ToLower(strings.Split(r.Host, ":")[0])
		if host == shareHost {
			shareHandler.ServeHTTP(w, r)
			return
		}
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")

		// CSRF defense-in-depth: block cross-site browser mutations on /api/* (excluding CLI agent enrollment).
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/agent/enroll" {
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
				if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
					writeError(w, http.StatusForbidden, "cross-site request blocked")
					return
				}
			}
		}

		appMux.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           rootHandler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("ourchive hub listening on http://localhost%s (share host: %s, db: %s)", addr, shareHost, dbPath)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("hub server error: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	log.Println("ourchive hub stopped")
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func newSPAHandler(webDir string) http.HandlerFunc {
	absRoot, _ := filepath.Abs(webDir)
	indexPath := filepath.Join(absRoot, "index.html")
	fsHandler := http.FileServer(http.Dir(absRoot))

	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		clean := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if clean == "." || clean == "" {
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeFile(w, r, indexPath)
			return
		}
		target := filepath.Join(absRoot, clean)
		if strings.HasPrefix(target, absRoot+string(os.PathSeparator)) {
			if info, err := os.Stat(target); err == nil && !info.IsDir() {
				if strings.HasPrefix(r.URL.Path, "/assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				fsHandler.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, indexPath)
	}
}

type ipBucket struct {
	tokens float64
	last   time.Time
}

type ipRateLimiter struct {
	mu          sync.Mutex
	buckets     map[string]*ipBucket
	ratePerSec  float64
	burst       float64
	lastCleanup time.Time
}

func newIPRateLimiter(perMinute, burst int) *ipRateLimiter {
	return &ipRateLimiter{
		buckets:     map[string]*ipBucket{},
		ratePerSec:  float64(perMinute) / 60.0,
		burst:       float64(burst),
		lastCleanup: time.Now(),
	}
}

func (l *ipRateLimiter) Allow(ip string) bool {
	if ip == "" {
		ip = "unknown"
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastCleanup) > 10*time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.last) > 15*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.lastCleanup = now
	}
	b, ok := l.buckets[ip]
	if !ok {
		l.buckets[ip] = &ipBucket{tokens: l.burst - 1, last: now}
		return true
	}
	b.tokens += now.Sub(b.last).Seconds() * l.ratePerSec
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens -= 1
	return true
}

func copyThrottled(w io.Writer, r io.Reader, capBytesPerSec int64) {
	buf := make([]byte, 32*1024)
	start := time.Now()
	var total int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			total += int64(n)
			expected := time.Duration(float64(total) / float64(capBytesPerSec) * float64(time.Second))
			if wait := expected - time.Since(start); wait > 0 {
				time.Sleep(wait)
			}
		}
		if err != nil {
			return
		}
	}
}

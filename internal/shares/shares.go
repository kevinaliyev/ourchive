package shares

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ourchive/ourchive/internal/audit"
	"github.com/ourchive/ourchive/internal/auth"
	"github.com/ourchive/ourchive/internal/db"
	"github.com/ourchive/ourchive/internal/settings"
	"github.com/ourchive/ourchive/internal/tunnel"
	"github.com/ourchive/ourchive/internal/vfs"
	"github.com/ourchive/ourchive/pkg/protocol"
)

const slugAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

var (
	ErrSlugTaken   = errors.New("that custom URL slug is already in use")
	ErrInvalidSlug = errors.New("custom slug must be 3–64 characters (letters, numbers, hyphens, underscores)")

	customSlugRe  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{1,62}[a-zA-Z0-9]$`)
	reservedSlugs = map[string]bool{
		"robots.txt": true, "favicon.ico": true, "logo.png": true, "api": true, "health": true,
		"raw": true, "oembed": true, "unlock": true, "zip": true,
		"thumb": true, "admin": true, "login": true, "static": true,
	}
)

// ValidateCustomSlug normalizes and validates a user-chosen share slug.
func ValidateCustomSlug(raw string) (string, error) {
	s := strings.TrimSpace(strings.TrimPrefix(raw, "/"))
	if !customSlugRe.MatchString(s) {
		return "", ErrInvalidSlug
	}
	if reservedSlugs[strings.ToLower(s)] {
		return "", errors.New("that custom URL slug is reserved")
	}
	return s, nil
}

type ShareLink struct {
	Slug        string              `json:"slug"`
	NodeID      string              `json:"node_id"`
	NodeName    string              `json:"node_name"`
	NodeStatus  protocol.NodeStatus `json:"node_status"`
	MountID     string              `json:"mount_id"`
	MountName   string              `json:"mount_name"`
	RelPath     string              `json:"rel_path"`
	IsDir       bool                `json:"is_dir"`
	Title       string              `json:"title"`
	MimeType    string              `json:"mime_type"`
	SizeBytes   int64               `json:"size_bytes"`
	HasPassword bool                `json:"has_password"`
	MaxViews    *int                `json:"max_views,omitempty"`
	ViewCount   int                 `json:"view_count"`
	ExpiresAt   string              `json:"expires_at,omitempty"`
	Expired     bool                `json:"expired"`
	CreatedBy   string              `json:"created_by"`
	CreatedAt   string              `json:"created_at"`
	URL         string              `json:"url"`

	passwordHash string
	fastHash     string
}

type CreateRequest struct {
	NodeID         string `json:"node_id"`
	MountID        string `json:"mount_id"`
	RelPath        string `json:"rel_path"`
	IsDir          bool   `json:"is_dir"`
	Title          string `json:"title"`
	MimeType       string `json:"mime_type"`
	SizeBytes      int64  `json:"size_bytes"`
	CustomSlug     string `json:"custom_slug,omitempty"`
	Password       string `json:"password,omitempty"`
	MaxViews       int    `json:"max_views,omitempty"`
	ExpiresInHours int    `json:"expires_in_hours,omitempty"` // 0 = permanent
}

type Service struct {
	store        *db.Store
	broker       *tunnel.Broker
	settings     *settings.Store
	audit        *audit.Service
	shareBaseURL string
	thumbDir     string
	cookieSecret []byte
	rateMu       sync.Mutex
	rateByIP     map[string]*ipBucket
}

// SetAudit attaches an audit.Service so public share link views, embeds, and downloads
// record the visitor IP address, user-agent, and referer in the Hub audit log.
func (s *Service) SetAudit(a *audit.Service) {
	s.audit = a
}

func isEmbedBotUA(ua string) bool {
	u := strings.ToLower(ua)
	for _, tok := range []string{"bot", "discord", "slack", "twitter", "telegram", "whatsapp", "facebookexternalhit", "embedly", "unfurl"} {
		if strings.Contains(u, tok) {
			return true
		}
	}
	return false
}

func (s *Service) logPublicAccess(r *http.Request, sl *ShareLink, action string) {
	if s.audit == nil || sl == nil || r == nil {
		return
	}
	ua := r.UserAgent()
	if len(ua) > 200 {
		ua = ua[:200]
	}
	isBot := isEmbedBotUA(ua)
	actor := "Public visitor"
	if isBot {
		actor = "Embed bot"
	}
	meta := map[string]any{
		"title":      sl.Title,
		"node_id":    sl.NodeID,
		"mount_id":   sl.MountID,
		"rel_path":   sl.RelPath,
		"user_agent": ua,
		"is_bot":     isBot,
	}
	if ref := strings.TrimSpace(r.Referer()); ref != "" {
		if len(ref) > 200 {
			ref = ref[:200]
		}
		meta["referer"] = ref
	}
	s.audit.Log(r.Context(), audit.Entry{
		Category:   audit.CategoryCore,
		Action:     action,
		ActorLabel: actor,
		TargetType: "share",
		TargetID:   sl.Slug,
		Metadata:   meta,
		IPAddress:  audit.ClientIP(r),
	})
}

type ipBucket struct {
	tokens float64
	last   time.Time
}

func NewService(store *db.Store, broker *tunnel.Broker, settingsStore *settings.Store, shareBaseURL, thumbDir string) *Service {
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	return &Service{
		store:        store,
		broker:       broker,
		settings:     settingsStore,
		shareBaseURL: strings.TrimRight(shareBaseURL, "/"),
		thumbDir:     thumbDir,
		cookieSecret: secret,
		rateByIP:     map[string]*ipBucket{},
	}
}

func generateSlug(n int) (string, error) {
	max := big.NewInt(int64(len(slugAlphabet)))
	var b strings.Builder
	for i := 0; i < n; i++ {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b.WriteByte(slugAlphabet[idx.Int64()])
	}
	return b.String(), nil
}

// IsSlugAvailable validates a custom slug and checks case-insensitively that no existing link uses it.
func (s *Service) IsSlugAvailable(ctx context.Context, raw string) (string, bool, string) {
	norm, err := ValidateCustomSlug(raw)
	if err != nil {
		return "", false, err.Error()
	}
	var count int
	if err := s.store.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM share_links WHERE LOWER(slug) = LOWER(?)`, norm).Scan(&count); err != nil {
		return norm, false, "database check failed"
	}
	if count > 0 {
		return norm, false, ErrSlugTaken.Error()
	}
	return norm, true, ""
}

func (s *Service) Create(ctx context.Context, req CreateRequest, userID string) (*ShareLink, error) {
	cleaned, err := vfs.CleanRelPath(req.RelPath)
	if err != nil {
		return nil, err
	}
	if req.NodeID == "" || req.MountID == "" {
		return nil, errors.New("node_id and mount_id are required")
	}

	var slug string
	if strings.TrimSpace(req.CustomSlug) != "" {
		norm, ok, reason := s.IsSlugAvailable(ctx, req.CustomSlug)
		if !ok {
			if reason == ErrSlugTaken.Error() {
				return nil, ErrSlugTaken
			}
			return nil, errors.New(reason)
		}
		slug = norm
	} else {
		slug, err = generateSlug(8)
		if err != nil {
			return nil, err
		}
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		if cleaned != "" {
			parts := strings.Split(cleaned, "/")
			title = parts[len(parts)-1]
		} else {
			title = "Shared folder"
		}
	}

	mimeType := strings.TrimSpace(req.MimeType)
	if req.IsDir {
		mimeType = "application/zip"
	} else if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	var pwHash sql.NullString
	if strings.TrimSpace(req.Password) != "" {
		h, err := auth.HashPassword(req.Password)
		if err != nil {
			return nil, err
		}
		pwHash = sql.NullString{String: h, Valid: true}
	}

	var maxViews sql.NullInt64
	if req.MaxViews > 0 {
		maxViews = sql.NullInt64{Int64: int64(req.MaxViews), Valid: true}
	}

	now := time.Now().UTC()
	var expiresAt sql.NullString
	if req.ExpiresInHours > 0 {
		expiresAt = sql.NullString{
			String: now.Add(time.Duration(req.ExpiresInHours) * time.Hour).Format(time.RFC3339),
			Valid:  true,
		}
	}

	isDirInt := 0
	if req.IsDir {
		isDirInt = 1
	}

	// Capture fast structural fingerprint from online Agent so moved/renamed files auto-heal.
	fastHash := ""
	sizeBytes := req.SizeBytes
	if !req.IsDir {
		if client, err := s.broker.HTTPClientForNode(req.NodeID); err == nil {
			q := url.Values{}
			q.Set("path", cleaned)
			if stReq, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agent/vfs/"+req.MountID+"/stat?"+q.Encode(), nil); err == nil {
				if stResp, err := client.Do(stReq); err == nil {
					if stResp.StatusCode == http.StatusOK {
						var st struct {
							SizeBytes int64  `json:"size_bytes"`
							FastHash  string `json:"fast_hash"`
						}
						if json.NewDecoder(stResp.Body).Decode(&st) == nil {
							if st.FastHash != "" {
								fastHash = st.FastHash
							}
							if st.SizeBytes > 0 {
								sizeBytes = st.SizeBytes
							}
						}
					}
					_ = stResp.Body.Close()
				}
			}
		}
	}

	_, err = s.store.DB.ExecContext(ctx, `
		INSERT INTO share_links
			(slug, node_id, mount_id, rel_path, is_dir, title, mime_type, size_bytes, fast_hash, password_hash, max_views, view_count, expires_at, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?)
	`, slug, req.NodeID, req.MountID, cleaned, isDirInt, title, mimeType, sizeBytes, fastHash, pwHash, maxViews, expiresAt, userID, now.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}

	return s.GetBySlug(ctx, slug)
}

func (s *Service) List(ctx context.Context, currentUser *auth.User) ([]ShareLink, error) {
	rows, err := s.store.DB.QueryContext(ctx, `
		SELECT sl.slug, sl.node_id, n.display_name, sl.mount_id, m.name, sl.rel_path, sl.is_dir,
		       sl.title, sl.mime_type, sl.size_bytes, sl.fast_hash, sl.password_hash, sl.max_views, sl.view_count,
		       sl.expires_at, sl.created_by, sl.created_at
		FROM share_links sl
		JOIN nodes n ON n.id = sl.node_id
		JOIN mounts m ON m.id = sl.mount_id
		ORDER BY sl.created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := []ShareLink{}
	for rows.Next() {
		link, err := scanShareRow(rows, s.shareBaseURL)
		if err != nil {
			return nil, err
		}
		list = append(list, link)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	nodes, _ := s.broker.ListNodesForUser(ctx, currentUser)
	statusByID := map[string]protocol.NodeStatus{}
	for _, n := range nodes {
		statusByID[n.ID] = n.Status
	}
	for i := range list {
		if st, ok := statusByID[list[i].NodeID]; ok {
			list[i].NodeStatus = st
		} else {
			list[i].NodeStatus = protocol.NodeStatusOffline
		}
	}
	return list, nil
}

// CanDelete returns true if the user created the share link, owns the hosting node, or is a host_admin.
func (s *Service) CanDelete(ctx context.Context, slug string, u *auth.User) (bool, error) {
	if u == nil {
		return false, nil
	}
	if u.Role == auth.RoleHostAdmin {
		return true, nil
	}
	var createdBy, nodeOwner string
	err := s.store.DB.QueryRowContext(ctx, `
		SELECT sl.created_by, n.owner_id
		FROM share_links sl
		JOIN nodes n ON n.id = sl.node_id
		WHERE sl.slug = ?
	`, slug).Scan(&createdBy, &nodeOwner)
	if errors.Is(err, sql.ErrNoRows) {
		return false, auth.ErrNotFound
	}
	if err != nil {
		return false, err
	}
	return createdBy == u.ID || nodeOwner == u.ID, nil
}

func (s *Service) GetBySlug(ctx context.Context, slug string) (*ShareLink, error) {
	row := s.store.DB.QueryRowContext(ctx, `
		SELECT sl.slug, sl.node_id, n.display_name, sl.mount_id, m.name, sl.rel_path, sl.is_dir,
		       sl.title, sl.mime_type, sl.size_bytes, sl.fast_hash, sl.password_hash, sl.max_views, sl.view_count,
		       sl.expires_at, sl.created_by, sl.created_at
		FROM share_links sl
		JOIN nodes n ON n.id = sl.node_id
		JOIN mounts m ON m.id = sl.mount_id
		WHERE sl.slug = ?
	`, slug)

	link, err := scanShareRow(row, s.shareBaseURL)
	if err != nil {
		return nil, err
	}
	if _, err := s.broker.HTTPClientForNode(link.NodeID); err == nil {
		link.NodeStatus = protocol.NodeStatusOnline
	} else {
		link.NodeStatus = protocol.NodeStatusOffline
	}
	return &link, nil
}

func (s *Service) Delete(ctx context.Context, slug string) error {
	_, err := s.store.DB.ExecContext(ctx, `DELETE FROM share_links WHERE slug = ?`, slug)
	return err
}

func (s *Service) incrementViews(ctx context.Context, slug string) {
	_, _ = s.store.DB.ExecContext(ctx, `UPDATE share_links SET view_count = view_count + 1 WHERE slug = ?`, slug)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanShareRow(r rowScanner, baseURL string) (ShareLink, error) {
	var sl ShareLink
	var isDirInt int
	var pwHash, expiresAt sql.NullString
	var maxViews sql.NullInt64

	if err := r.Scan(
		&sl.Slug, &sl.NodeID, &sl.NodeName, &sl.MountID, &sl.MountName, &sl.RelPath, &isDirInt,
		&sl.Title, &sl.MimeType, &sl.SizeBytes, &sl.fastHash, &pwHash, &maxViews, &sl.ViewCount,
		&expiresAt, &sl.CreatedBy, &sl.CreatedAt,
	); err != nil {
		return ShareLink{}, err
	}

	sl.IsDir = isDirInt == 1
	sl.HasPassword = pwHash.Valid && pwHash.String != ""
	sl.passwordHash = pwHash.String
	if maxViews.Valid {
		mv := int(maxViews.Int64)
		sl.MaxViews = &mv
	}
	if expiresAt.Valid {
		sl.ExpiresAt = expiresAt.String
		if exp, err := time.Parse(time.RFC3339, expiresAt.String); err == nil && time.Now().UTC().After(exp) {
			sl.Expired = true
		}
	}
	if sl.MaxViews != nil && sl.ViewCount >= *sl.MaxViews {
		sl.Expired = true
	}
	sl.URL = baseURL + "/" + sl.Slug
	return sl, nil
}

// --- Isolated Share Subdomain HTTP Handler ---

func (s *Service) ShareHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /\n"))
	})
	mux.HandleFunc("GET /logo.png", s.handleBrandLogo)
	mux.HandleFunc("GET /favicon.ico", s.handleBrandLogo)
	mux.HandleFunc("GET /{slug}", s.handleSharePage)
	mux.HandleFunc("POST /{slug}/unlock", s.handleUnlock)
	mux.HandleFunc("GET /{slug}/oembed", s.handleOEmbed)
	mux.HandleFunc("GET /{slug}/thumb.webp", s.handleThumb)
	mux.HandleFunc("GET /{slug}/raw", s.handleRaw)
	mux.HandleFunc("HEAD /{slug}/raw", s.handleRaw)
	return mux
}

var (
	brandLogoOnce sync.Once
	brandLogoPNG  []byte
)

// generateBrandLogoPNG renders the Web UI .brand-mark logo (warm light rounded square
// with three dark horizontal bars) as a crisp antialiased PNG for favicons and embeds.
func generateBrandLogoPNG(size int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	fg := color.NRGBA{R: 239, G: 233, B: 222, A: 255} // #efe9de (--text-primary)
	bg := color.NRGBA{R: 22, G: 21, B: 19, A: 255}    // #161513 (--bg-canvas)

	s := float64(size)
	radius := s * 0.25 // 6px on 24px box

	// 14/24 icon inset, matching .brand-mark (24px box, 14px SVG with viewBox 0 0 24 24, strokeWidth 2).
	iconSize := s * (14.0 / 24.0)
	iconOff := (s - iconSize) / 2.0
	scale := iconSize / 24.0
	halfStroke := 1.0 * scale // strokeWidth=2 -> half=1

	type seg struct{ x1, x2, y float64 }
	segs := []seg{
		{4, 20, 7},
		{4, 20, 12},
		{4, 14, 17},
	}

	// Distance from point (px, py) to rounded rectangle [0, s] x [0, s] with corner radius.
	rrectSDF := func(px, py float64) float64 {
		cx := s / 2.0
		cy := s / 2.0
		qx := math.Abs(px-cx) - (cx - radius)
		qy := math.Abs(py-cy) - (cy - radius)
		outX := math.Max(qx, 0)
		outY := math.Max(qy, 0)
		return math.Hypot(outX, outY) + math.Min(math.Max(qx, qy), 0) - radius
	}

	// Distance from point (px, py) to horizontal capsule segment.
	segSDF := func(px, py float64, sg seg) float64 {
		ax := iconOff + sg.x1*scale
		bx := iconOff + sg.x2*scale
		sy := iconOff + sg.y*scale
		clampedX := math.Max(ax, math.Min(bx, px))
		return math.Hypot(px-clampedX, py-sy) - halfStroke
	}

	for y := 0; y < size; y++ {
		py := float64(y) + 0.5
		for x := 0; x < size; x++ {
			px := float64(x) + 0.5
			dBox := rrectSDF(px, py)
			if dBox >= 0.75 {
				continue
			}
			boxCov := math.Max(0, math.Min(1, 0.5-dBox))

			dBar := 1e9
			for _, sg := range segs {
				if d := segSDF(px, py, sg); d < dBar {
					dBar = d
				}
			}
			barCov := math.Max(0, math.Min(1, 0.5-dBar))

			r := uint8(math.Round(float64(fg.R)*(1-barCov) + float64(bg.R)*barCov))
			g := uint8(math.Round(float64(fg.G)*(1-barCov) + float64(bg.G)*barCov))
			b := uint8(math.Round(float64(fg.B)*(1-barCov) + float64(bg.B)*barCov))
			a := uint8(math.Round(255.0 * boxCov))
			img.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: a})
		}
	}

	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func (s *Service) handleBrandLogo(w http.ResponseWriter, r *http.Request) {
	brandLogoOnce.Do(func() {
		brandLogoPNG = generateBrandLogoPNG(128)
	})
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(brandLogoPNG)
}

func (s *Service) allowIP(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	now := time.Now()
	b, ok := s.rateByIP[host]
	if !ok {
		b = &ipBucket{tokens: 60, last: now}
		s.rateByIP[host] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * 5.0 // 5 req/sec refill, burst 60
	if b.tokens > 60 {
		b.tokens = 60
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens -= 1
	return true
}

func (s *Service) signUnlockCookie(slug string) string {
	mac := hmac.New(sha256.New, s.cookieSecret)
	mac.Write([]byte("unlock:" + slug))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Service) isUnlocked(r *http.Request, sl *ShareLink) bool {
	if !sl.HasPassword {
		return true
	}
	c, err := r.Cookie("ourchive_unlock_" + sl.Slug)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(s.signUnlockCookie(sl.Slug)))
}

func (s *Service) handleUnlock(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	sl, err := s.GetBySlug(r.Context(), slug)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	pw := r.FormValue("password")
	if !auth.VerifyPassword(pw, sl.passwordHash) {
		http.Redirect(w, r, "/"+slug+"?err=1", http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "ourchive_unlock_" + slug,
		Value:    s.signUnlockCookie(slug),
		Path:     "/" + slug,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   3600,
	})
	http.Redirect(w, r, "/"+slug, http.StatusSeeOther)
}

func (s *Service) handleSharePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	slug := r.PathValue("slug")
	sl, err := s.GetBySlug(r.Context(), slug)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(renderSimplePage("Not found", "This share link does not exist or has been revoked.")))
		return
	}
	if sl.Expired {
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(renderSimplePage("Link expired", "This share link has expired or reached its view limit.")))
		return
	}

	if !s.isUnlocked(r, sl) {
		errBanner := ""
		if r.URL.Query().Get("err") == "1" {
			errBanner = `<p class="err">Incorrect password. Please try again.</p>`
		}
		_, _ = w.Write([]byte(fmt.Sprintf(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex,nofollow"><link rel="icon" type="image/png" href="/logo.png"><title>%s · ourchive</title><style>%s</style></head>
<body><main class="card"><div class="brand">%s<span>ourchive</span></div><h1>%s</h1><p class="sub">This shared item is password protected.</p>%s
<form method="post" action="/%s/unlock" class="pw-form">
<input type="password" name="password" placeholder="Enter password" required autofocus>
<button type="submit" class="btn-primary">Unlock</button>
</form></main></body></html>`,
			html.EscapeString(sl.Title), sharePageCSS, shareBrandMarkHTML, html.EscapeString(sl.Title), errBanner, html.EscapeString(slug))))
		return
	}

	// Don't double-count bot preview fetches as views.
	if !isEmbedBotUA(r.UserAgent()) {
		s.incrementViews(r.Context(), slug)
	}
	s.logPublicAccess(r, sl, "share.link_viewed")

	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	origin := scheme + "://" + r.Host
	pageURL := origin + "/" + slug
	rawURL := pageURL + "/raw"
	thumbURL := pageURL + "/thumb.webp"
	logoURL := origin + "/logo.png"
	oembedURL := pageURL + "/oembed"

	isVideo := strings.HasPrefix(sl.MimeType, "video/")
	isAudio := strings.HasPrefix(sl.MimeType, "audio/")
	isImage := strings.HasPrefix(sl.MimeType, "image/")
	isOnline := sl.NodeStatus == protocol.NodeStatusOnline

	var metaTags strings.Builder
	fmt.Fprintf(&metaTags, `<link rel="icon" type="image/png" href="%s">`, html.EscapeString(logoURL))
	fmt.Fprintf(&metaTags, `<meta name="theme-color" content="#efe9de">`)
	fmt.Fprintf(&metaTags, `<meta property="og:site_name" content="ourchive">`)
	fmt.Fprintf(&metaTags, `<meta property="og:title" content="%s">`, html.EscapeString(sl.Title))
	fmt.Fprintf(&metaTags, `<meta property="og:url" content="%s">`, html.EscapeString(pageURL))
	fmt.Fprintf(&metaTags, `<link rel="alternate" type="application/json+oembed" href="%s" title="%s">`, html.EscapeString(oembedURL), html.EscapeString(sl.Title))

	desc := fmt.Sprintf("Hosted on %s", sl.NodeName)
	if sl.IsDir {
		desc = fmt.Sprintf("Shared folder (ZIP download) · Hosted on %s", sl.NodeName)
	} else if sl.SizeBytes > 0 {
		desc = fmt.Sprintf("%s · Hosted on %s", humanSize(sl.SizeBytes), sl.NodeName)
	}
	fmt.Fprintf(&metaTags, `<meta property="og:description" content="%s">`, html.EscapeString(desc))

	if isVideo {
		fmt.Fprintf(&metaTags, `<meta property="og:type" content="video.other">`)
		fmt.Fprintf(&metaTags, `<meta property="og:image" content="%s">`, html.EscapeString(thumbURL))
		fmt.Fprintf(&metaTags, `<meta property="og:video" content="%s">`, html.EscapeString(rawURL))
		if scheme == "https" {
			fmt.Fprintf(&metaTags, `<meta property="og:video:secure_url" content="%s">`, html.EscapeString(rawURL))
		}
		fmt.Fprintf(&metaTags, `<meta property="og:video:type" content="%s">`, html.EscapeString(sl.MimeType))
		fmt.Fprintf(&metaTags, `<meta property="og:video:width" content="1280">`)
		fmt.Fprintf(&metaTags, `<meta property="og:video:height" content="720">`)
		fmt.Fprintf(&metaTags, `<meta name="twitter:card" content="player">`)
		fmt.Fprintf(&metaTags, `<meta name="twitter:player:stream" content="%s">`, html.EscapeString(rawURL))
	} else if isImage {
		fmt.Fprintf(&metaTags, `<meta property="og:type" content="website">`)
		fmt.Fprintf(&metaTags, `<meta property="og:image" content="%s">`, html.EscapeString(thumbURL))
		fmt.Fprintf(&metaTags, `<meta name="twitter:card" content="summary_large_image">`)
	} else {
		fmt.Fprintf(&metaTags, `<meta property="og:type" content="website">`)
		fmt.Fprintf(&metaTags, `<meta property="og:image" content="%s">`, html.EscapeString(logoURL))
		fmt.Fprintf(&metaTags, `<meta name="twitter:card" content="summary">`)
	}

	var bodyHTML string
	if !isOnline {
		bodyHTML = fmt.Sprintf(`<div class="offline-banner">Host machine <strong>%s</strong> is currently offline. Check back when it reconnects.</div>`, html.EscapeString(sl.NodeName))
		if isImage || isVideo {
			bodyHTML += fmt.Sprintf(`<div class="stage"><img src="%s" alt="%s" onerror="this.style.display='none'"></div>`, html.EscapeString(thumbURL), html.EscapeString(sl.Title))
		}
	} else if sl.IsDir {
		bodyHTML = fmt.Sprintf(`<div class="zip-box"><p>Folder archive streamed directly from <strong>%s</strong>.</p><a class="btn-primary" href="%s?download=1">Download ZIP</a></div>`,
			html.EscapeString(sl.NodeName), html.EscapeString(rawURL))
	} else if isVideo {
		bodyHTML = fmt.Sprintf(`<div class="stage"><video controls playsinline preload="metadata" poster="%s" src="%s"></video></div>`,
			html.EscapeString(thumbURL), html.EscapeString(rawURL))
	} else if isImage {
		bodyHTML = fmt.Sprintf(`<div class="stage"><img src="%s" alt="%s"></div>`, html.EscapeString(rawURL), html.EscapeString(sl.Title))
	} else if isAudio {
		bodyHTML = fmt.Sprintf(`<div class="zip-box"><audio controls preload="metadata" src="%s" style="width:100%%"></audio></div>`, html.EscapeString(rawURL))
	} else {
		bodyHTML = fmt.Sprintf(`<div class="zip-box"><p>%s</p><a class="btn-primary" href="%s?download=1">Download file</a></div>`,
			html.EscapeString(desc), html.EscapeString(rawURL))
	}

	dlBtn := ""
	if isOnline && !sl.IsDir {
		dlBtn = fmt.Sprintf(`<a class="btn-primary" href="%s?download=1">Download</a>`, html.EscapeString(rawURL))
	}

	_, _ = w.Write([]byte(fmt.Sprintf(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex,nofollow">%s<title>%s · ourchive</title><style>%s</style></head>
<body><main class="wrap"><div class="brand">%s<span>ourchive</span></div><header class="head"><div><h1>%s</h1><p class="sub">%s</p></div>%s</header>%s</main></body></html>`,
		metaTags.String(), html.EscapeString(sl.Title), sharePageCSS, shareBrandMarkHTML,
		html.EscapeString(sl.Title), html.EscapeString(desc), dlBtn, bodyHTML)))
}

func (s *Service) handleOEmbed(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	sl, err := s.GetBySlug(r.Context(), slug)
	if err != nil || sl.Expired || sl.HasPassword {
		http.NotFound(w, r)
		return
	}
	s.logPublicAccess(r, sl, "share.embed_fetched")
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	origin := scheme + "://" + r.Host
	typ := "link"
	thumbURL := origin + "/logo.png"
	if strings.HasPrefix(sl.MimeType, "video/") {
		typ = "video"
		thumbURL = origin + "/" + slug + "/thumb.webp"
	} else if strings.HasPrefix(sl.MimeType, "image/") {
		typ = "photo"
		thumbURL = origin + "/" + slug + "/thumb.webp"
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"version":       "1.0",
		"type":          typ,
		"title":         sl.Title,
		"provider_name": "ourchive",
		"thumbnail_url": thumbURL,
	})
}

func (s *Service) handleThumb(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	sl, err := s.GetBySlug(r.Context(), slug)
	if err != nil || sl.Expired || sl.HasPassword || sl.IsDir {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	s.logPublicAccess(r, sl, "share.embed_thumb")

	sum := sha256.Sum256([]byte(sl.NodeID + "|" + sl.MountID + "|" + sl.RelPath))
	cacheFile := filepath.Join(s.thumbDir, hex.EncodeToString(sum[:16])+".webp")
	if data, err := os.ReadFile(cacheFile); err == nil && len(data) > 0 {
		w.Header().Set("Content-Type", "image/webp")
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		_, _ = w.Write(data)
		return
	}

	client, err := s.broker.HTTPClientForNode(sl.NodeID)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	q := url.Values{}
	q.Set("path", sl.RelPath)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://agent/vfs/"+sl.MountID+"/thumb?"+q.Encode(), nil)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil || len(body) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "webp") && len(body) <= 512*1024 {
		_ = os.WriteFile(cacheFile, body, 0o644)
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	_, _ = w.Write(body)
}

func (s *Service) handleRaw(w http.ResponseWriter, r *http.Request) {
	if !s.allowIP(r) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	slug := r.PathValue("slug")
	sl, err := s.GetBySlug(r.Context(), slug)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if sl.Expired {
		http.Error(w, "share link expired", http.StatusGone)
		return
	}
	if !s.isUnlocked(r, sl) {
		http.Error(w, "password required", http.StatusUnauthorized)
		return
	}

	if r.Method == http.MethodGet {
		rangeHdr := r.Header.Get("Range")
		if rangeHdr == "" || strings.HasPrefix(rangeHdr, "bytes=0-") {
			act := "share.embed_streamed"
			if sl.IsDir || r.URL.Query().Get("download") == "1" {
				act = "share.downloaded"
			}
			s.logPublicAccess(r, sl, act)
		}
	}

	// Enforce Host Admin public share relay policy.
	if s.settings != nil {
		pol := s.settings.Get()
		if !pol.AllowShareRelay {
			http.Error(w, "public share relaying is disabled by the Hub administrator", http.StatusForbidden)
			return
		}
		if pol.MaxRelayFileMB > 0 && sl.SizeBytes > int64(pol.MaxRelayFileMB)*1024*1024 {
			http.Error(w, fmt.Sprintf("file exceeds Hub relay limit (%d MB)", pol.MaxRelayFileMB), http.StatusRequestEntityTooLarge)
			return
		}
	}

	client, err := s.broker.HTTPClientForNode(sl.NodeID)
	if err != nil {
		http.Error(w, "host machine is offline", http.StatusServiceUnavailable)
		return
	}

	q := url.Values{}
	q.Set("path", sl.RelPath)
	targetURL := "http://agent/vfs/" + sl.MountID + "/file?" + q.Encode()
	if sl.IsDir {
		targetURL = "http://agent/vfs/" + sl.MountID + "/zip?" + q.Encode()
	} else if r.URL.Query().Get("download") == "1" {
		q.Set("download", "1")
		targetURL = "http://agent/vfs/" + sl.MountID + "/file?" + q.Encode()
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, nil)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	for _, hdr := range []string{"Range", "If-Range", "If-Modified-Since", "If-None-Match"} {
		if v := r.Header.Get(hdr); v != "" {
			req.Header.Set(hdr, v)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "agent stream failed", http.StatusBadGateway)
		return
	}

	// Auto-heal moved or renamed files on disk using fast structural fingerprint!
	if resp.StatusCode == http.StatusNotFound && !sl.IsDir && sl.fastHash != "" {
		_ = resp.Body.Close()
		if newMount, newPath, ok := s.tryRelocate(r.Context(), client, sl); ok {
			sl.MountID = newMount
			sl.RelPath = newPath
			q2 := url.Values{}
			q2.Set("path", newPath)
			if r.URL.Query().Get("download") == "1" {
				q2.Set("download", "1")
			}
			retryReq, err := http.NewRequestWithContext(r.Context(), r.Method, "http://agent/vfs/"+newMount+"/file?"+q2.Encode(), nil)
			if err == nil {
				for _, hdr := range []string{"Range", "If-Range", "If-Modified-Since", "If-None-Match"} {
					if v := r.Header.Get(hdr); v != "" {
						retryReq.Header.Set(hdr, v)
					}
				}
				if retryResp, err := client.Do(retryReq); err == nil {
					resp = retryResp
				}
			}
		} else {
			http.NotFound(w, r)
			return
		}
	}
	defer resp.Body.Close()

	for _, hdr := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Content-Disposition", "Last-Modified", "ETag"} {
		if v := resp.Header.Get(hdr); v != "" {
			w.Header().Set(hdr, v)
		}
	}
	// Neutralize stored XSS in uploaded HTML/XHTML/SVG files.
	ctLower := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ctLower, "text/html") || strings.Contains(ctLower, "application/xhtml") || strings.Contains(ctLower, "image/svg") {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filepath.Base(sl.RelPath)))
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, resp.Body)
	}
}

func (s *Service) tryRelocate(ctx context.Context, client *http.Client, sl *ShareLink) (string, string, bool) {
	body, _ := json.Marshal(map[string]any{
		"size_bytes": sl.SizeBytes,
		"fast_hash":  sl.fastHash,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent/vfs/relocate", strings.NewReader(string(body)))
	if err != nil {
		return "", "", false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", false
	}
	var res struct {
		Found   bool   `json:"found"`
		MountID string `json:"mount_id"`
		RelPath string `json:"rel_path"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil || !res.Found {
		return "", "", false
	}
	// Persist the healed pointer across share_links and tagged_items.
	_, _ = s.store.DB.ExecContext(ctx, `UPDATE share_links SET mount_id = ?, rel_path = ? WHERE slug = ?`, res.MountID, res.RelPath, sl.Slug)
	_, _ = s.store.DB.ExecContext(ctx, `UPDATE tagged_items SET mount_id = ?, rel_path = ? WHERE node_id = ? AND mount_id = ? AND rel_path = ?`,
		res.MountID, res.RelPath, sl.NodeID, sl.MountID, sl.RelPath)
	return res.MountID, res.RelPath, true
}

func renderSimplePage(title, message string) string {
	return fmt.Sprintf(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex,nofollow"><link rel="icon" type="image/png" href="/logo.png"><title>%s · ourchive</title><style>%s</style></head><body><main class="card"><div class="brand">%s<span>ourchive</span></div><h1>%s</h1><p class="sub">%s</p></main></body></html>`,
		html.EscapeString(title), sharePageCSS, shareBrandMarkHTML, html.EscapeString(title), html.EscapeString(message))
}

const shareBrandMarkHTML = `<span class="brand-mark" aria-hidden="true"><svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M4 7h16M4 12h16M4 17h10"/></svg></span>`

func humanSize(b int64) string {
	if b < 1024 {
		return fmt.Sprintf("%d B", b)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(b)
	i := -1
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

const sharePageCSS = `
*{box-sizing:border-box}
body{margin:0;min-height:100dvh;background:#161513;color:#efe9de;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;display:grid;place-items:center;padding:24px}
.wrap{width:min(960px,100%);display:flex;flex-direction:column;gap:16px}
.card{width:min(440px,100%);background:#1c1a18;border:1px solid rgba(239,233,222,.09);border-radius:12px;padding:24px}
.head{display:flex;align-items:center;justify-content:space-between;gap:16px;flex-wrap:wrap}
.brand{display:inline-flex;align-items:center;gap:9px;font-size:14px;font-weight:500;letter-spacing:-.01em;color:#efe9de;margin-bottom:4px}
.brand-mark{width:24px;height:24px;border-radius:6px;background:#efe9de;color:#161513;display:grid;place-items:center}
h1{margin:0;font-size:20px;font-weight:500;letter-spacing:-.015em}
.sub{margin:4px 0 0;color:#aaa398;font-size:13px}
.stage{background:#100f0e;border:1px solid rgba(239,233,222,.08);border-radius:12px;overflow:hidden;display:grid;place-items:center;min-height:240px}
.stage video,.stage img{max-width:100%;max-height:76vh;display:block}
.zip-box{background:#1c1a18;border:1px solid rgba(239,233,222,.08);border-radius:12px;padding:28px;display:flex;flex-direction:column;align-items:center;gap:14px;text-align:center}
.offline-banner{padding:12px 14px;border-radius:8px;background:rgba(238,140,140,.12);border:1px solid rgba(238,140,140,.45);color:#ee8c8c;font-size:13px}
.btn-primary{display:inline-flex;align-items:center;justify-content:center;height:36px;padding:0 16px;border-radius:8px;text-decoration:none;font-size:13px;font-weight:500;color:#1f1b16;border:1px solid rgba(31,27,22,.16);background:linear-gradient(118deg,#f5ece6 0%,#f5eedc 28%,#e4efe9 56%,#e3ecf6 80%,#eee5f4 100%);cursor:pointer}
.pw-form{display:flex;gap:8px;margin-top:16px}
.pw-form input{flex:1;height:36px;padding:0 11px;border-radius:8px;border:1px solid rgba(239,233,222,.16);background:#161513;color:#efe9de}
.err{color:#ee8c8c;font-size:13px;margin:8px 0 0}
`

// Package auth — OpenID Connect (OIDC) discovery, PKCE (S256) authorization code
// exchange, email account linking, and invite-gated user registration.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ourchive/ourchive/internal/settings"
)

type oidcDiscovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

type oidcState struct {
	ProviderID   string
	CodeVerifier string
	InviteCode   string
	ExpiresAt    time.Time
}

// OIDCManager handles OpenID Connect discovery, PKCE authorization flows, and user linking.
type OIDCManager struct {
	authSvc  *Service
	settings *settings.Store
	httpCli  *http.Client

	mu       sync.Mutex
	states   map[string]oidcState
	discCache map[string]oidcDiscovery
}

func NewOIDCManager(authSvc *Service, settingsStore *settings.Store) *OIDCManager {
	return &OIDCManager{
		authSvc:   authSvc,
		settings:  settingsStore,
		httpCli:   &http.Client{Timeout: 12 * time.Second},
		states:    map[string]oidcState{},
		discCache: map[string]oidcDiscovery{},
	}
}

func (m *OIDCManager) findProvider(id string) (settings.OIDCProviderConfig, bool) {
	pol := m.settings.Get()
	for _, p := range pol.OIDCProviders {
		if p.Enabled && strings.EqualFold(p.ID, id) {
			return p, true
		}
	}
	return settings.OIDCProviderConfig{}, false
}

func (m *OIDCManager) discover(ctx context.Context, issuer string) (oidcDiscovery, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	if issuer == "" {
		return oidcDiscovery{}, errors.New("oidc issuer URL is empty")
	}

	m.mu.Lock()
	if cached, ok := m.discCache[issuer]; ok {
		m.mu.Unlock()
		return cached, nil
	}
	m.mu.Unlock()

	wellKnown := issuer + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnown, nil)
	if err != nil {
		return oidcDiscovery{}, err
	}
	resp, err := m.httpCli.Do(req)
	if err != nil {
		return oidcDiscovery{}, fmt.Errorf("oidc discovery failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return oidcDiscovery{}, fmt.Errorf("oidc discovery returned HTTP %d", resp.StatusCode)
	}
	var doc oidcDiscovery
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return oidcDiscovery{}, err
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" {
		return oidcDiscovery{}, errors.New("oidc discovery missing authorization or token endpoint")
	}

	m.mu.Lock()
	m.discCache[issuer] = doc
	m.mu.Unlock()
	return doc, nil
}

func randomB64URL(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// BeginLogin returns the authorization redirect URL for the requested OIDC provider.
func (m *OIDCManager) BeginLogin(ctx context.Context, providerID, callbackURL, inviteCode string) (string, error) {
	prov, ok := m.findProvider(providerID)
	if !ok {
		return "", errors.New("OIDC provider not found or disabled")
	}
	doc, err := m.discover(ctx, prov.IssuerURL)
	if err != nil {
		return "", err
	}

	state := randomB64URL(24)
	verifier := randomB64URL(32)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	m.mu.Lock()
	now := time.Now()
	for k, v := range m.states {
		if now.After(v.ExpiresAt) {
			delete(m.states, k)
		}
	}
	m.states[state] = oidcState{
		ProviderID:   prov.ID,
		CodeVerifier: verifier,
		InviteCode:   strings.TrimSpace(inviteCode),
		ExpiresAt:    now.Add(10 * time.Minute),
	}
	m.mu.Unlock()

	scopes := strings.TrimSpace(prov.Scopes)
	if scopes == "" {
		scopes = "openid email profile"
	}

	u, err := url.Parse(doc.AuthorizationEndpoint)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", prov.ClientID)
	q.Set("redirect_uri", callbackURL)
	q.Set("scope", scopes)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// CompleteCallback exchanges the authorization code, looks up or links the user,
// and returns the authenticated User (enforcing invite-only registration for new users).
func (m *OIDCManager) CompleteCallback(ctx context.Context, providerID, code, state, callbackURL string) (*User, error) {
	m.mu.Lock()
	st, ok := m.states[state]
	if ok {
		delete(m.states, state)
	}
	m.mu.Unlock()
	if !ok || time.Now().After(st.ExpiresAt) || !strings.EqualFold(st.ProviderID, providerID) {
		return nil, errors.New("invalid or expired OIDC state")
	}

	prov, ok := m.findProvider(providerID)
	if !ok {
		return nil, errors.New("OIDC provider not found or disabled")
	}
	doc, err := m.discover(ctx, prov.IssuerURL)
	if err != nil {
		return nil, err
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", callbackURL)
	form.Set("client_id", prov.ClientID)
	if prov.ClientSecret != "" {
		form.Set("client_secret", prov.ClientSecret)
	}
	form.Set("code_verifier", st.CodeVerifier)

	tokReq, err := http.NewRequestWithContext(ctx, http.MethodPost, doc.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	tokReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokReq.Header.Set("Accept", "application/json")
	tokResp, err := m.httpCli.Do(tokReq)
	if err != nil {
		return nil, fmt.Errorf("token exchange failed: %w", err)
	}
	defer tokResp.Body.Close()
	if tokResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint returned HTTP %d", tokResp.StatusCode)
	}
	var tokData struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
	}
	if err := json.NewDecoder(io.LimitReader(tokResp.Body, 1<<20)).Decode(&tokData); err != nil {
		return nil, err
	}

	// Fetch userinfo using the access token.
	var subject, email, name string
	if doc.UserinfoEndpoint != "" && tokData.AccessToken != "" {
		uiReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, doc.UserinfoEndpoint, nil)
		uiReq.Header.Set("Authorization", "Bearer "+tokData.AccessToken)
		if uiResp, err := m.httpCli.Do(uiReq); err == nil {
			defer uiResp.Body.Close()
			if uiResp.StatusCode == http.StatusOK {
				var claims struct {
					Sub               string `json:"sub"`
					Email             string `json:"email"`
					Name              string `json:"name"`
					PreferredUsername string `json:"preferred_username"`
				}
				if json.NewDecoder(io.LimitReader(uiResp.Body, 1<<20)).Decode(&claims) == nil {
					subject = strings.TrimSpace(claims.Sub)
					email = strings.ToLower(strings.TrimSpace(claims.Email))
					name = strings.TrimSpace(claims.Name)
					if name == "" {
						name = strings.TrimSpace(claims.PreferredUsername)
					}
				}
			}
		}
	}

	if subject == "" {
		return nil, errors.New("OIDC provider did not return a user subject")
	}

	// 1. Check existing oidc_links mapping.
	var existingUserID string
	err = m.authSvc.store.DB.QueryRowContext(ctx, `
		SELECT user_id FROM oidc_links WHERE provider = ? AND subject = ?
	`, prov.ID, subject).Scan(&existingUserID)
	if err == nil && existingUserID != "" {
		return m.authSvc.GetUserByID(ctx, existingUserID)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// 2. Link to an existing user with the same verified email.
	if email != "" {
		var matchedID string
		err := m.authSvc.store.DB.QueryRowContext(ctx, `SELECT id FROM users WHERE LOWER(email) = LOWER(?)`, email).Scan(&matchedID)
		if err == nil && matchedID != "" {
			_, _ = m.authSvc.store.DB.ExecContext(ctx, `
				INSERT OR IGNORE INTO oidc_links (id, provider, subject, user_id, email, created_at)
				VALUES (?, ?, ?, ?, ?, ?)
			`, uuid.NewString(), prov.ID, subject, matchedID, email, time.Now().UTC().Format(time.RFC3339))
			return m.authSvc.GetUserByID(ctx, matchedID)
		}
	}

	// 3. New user: require an invite key (maintains strict invite-only policy).
	if st.InviteCode == "" {
		return nil, errors.New("no existing Ourchive account matches this login — paste your invite key first to register")
	}
	if name == "" {
		name = email
		if name == "" {
			name = "Member"
		}
	}
	randomPass := randomB64URL(24)
	u, _, err := m.authSvc.RedeemInviteAndRegister(ctx, st.InviteCode, email, "", name, randomPass, settings.IdentityEmailOrUsername)
	if err != nil {
		return nil, err
	}
	_, _ = m.authSvc.store.DB.ExecContext(ctx, `
		INSERT OR IGNORE INTO oidc_links (id, provider, subject, user_id, email, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, uuid.NewString(), prov.ID, subject, u.ID, email, time.Now().UTC().Format(time.RFC3339))
	return u, nil
}

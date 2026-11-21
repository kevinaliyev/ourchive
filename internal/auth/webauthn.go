// Package auth — WebAuthn / Passkey registration, discoverable authentication
// (Conditional UI + explicit button), and credential CRUD via go-webauthn.
package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// StoredPasskeyCredential is the public metadata returned to the Passkey Management UI.
type StoredPasskeyCredential struct {
	ID                   string   `json:"id"` // Unpadded Base64URL credential ID
	PasskeyUserID        string   `json:"passkeyUserId"`
	Name                 string   `json:"name"`
	AAGUID               string   `json:"aaguid"`
	CredentialDeviceType string   `json:"credentialDeviceType"`
	CredentialBackedUp   bool     `json:"credentialBackedUp"`
	Transports           []string `json:"transports"`
	RegisteredAt         int64    `json:"registeredAt"`
	LastUsedAt           *int64   `json:"lastUsedAt,omitempty"`
	Counter              uint32   `json:"counter"`
}

// PasskeyUserID returns the unpadded Base64URL representation of a user's ID
// required by WebAuthn and the WebAuthn Signal API.
func PasskeyUserID(userID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(userID))
}

type webauthnUser struct {
	user  *User
	creds []webauthn.Credential
}

func (u *webauthnUser) WebAuthnID() []byte { return []byte(u.user.ID) }
func (u *webauthnUser) WebAuthnName() string {
	if u.user.Username != "" {
		return u.user.Username
	}
	if u.user.Email != "" {
		return u.user.Email
	}
	return u.user.DisplayName
}
func (u *webauthnUser) WebAuthnDisplayName() string { return u.user.DisplayName }
func (u *webauthnUser) WebAuthnCredentials() []webauthn.Credential {
	return u.creds
}

type pendingCeremony struct {
	Session   *webauthn.SessionData
	UserID    string
	ExpiresAt time.Time
}

// PasskeyManager wraps go-webauthn for phishing-resistant passkey registration,
// discoverable authentication (Conditional UI + button), and credential CRUD.
type PasskeyManager struct {
	authSvc *Service
	wa      *webauthn.WebAuthn
	rpID    string
	rpName  string

	mu       sync.Mutex
	sessions map[string]pendingCeremony
}

func NewPasskeyManager(authSvc *Service, publicHubURL string) (*PasskeyManager, error) {
	rpID := "localhost"
	origins := []string{"http://localhost:8080", "http://localhost:5173"}
	if u, err := url.Parse(strings.TrimSpace(publicHubURL)); err == nil && u.Hostname() != "" {
		rpID = u.Hostname()
		origin := u.Scheme + "://" + u.Host
		found := false
		for _, o := range origins {
			if o == origin {
				found = true
				break
			}
		}
		if !found {
			origins = append([]string{origin}, origins...)
		}
	}

	wa, err := webauthn.New(&webauthn.Config{
		RPDisplayName: "Ourchive",
		RPID:          rpID,
		RPOrigins:     origins,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationPreferred,
		},
	})
	if err != nil {
		return nil, err
	}

	return &PasskeyManager{
		authSvc:  authSvc,
		wa:       wa,
		rpID:     rpID,
		rpName:   "Ourchive",
		sessions: map[string]pendingCeremony{},
	}, nil
}

func (m *PasskeyManager) RPID() string { return m.rpID }

func (m *PasskeyManager) storeCeremony(userID string, sess *webauthn.SessionData) string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)
	now := time.Now()
	m.mu.Lock()
	for k, v := range m.sessions {
		if now.After(v.ExpiresAt) {
			delete(m.sessions, k)
		}
	}
	m.sessions[token] = pendingCeremony{
		Session:   sess,
		UserID:    userID,
		ExpiresAt: now.Add(5 * time.Minute),
	}
	m.mu.Unlock()
	return token
}

func (m *PasskeyManager) popCeremony(token string) (pendingCeremony, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.sessions[token]
	if ok {
		delete(m.sessions, token)
	}
	if !ok || time.Now().After(c.ExpiresAt) {
		return pendingCeremony{}, false
	}
	return c, true
}

func (m *PasskeyManager) loadCredentials(ctx context.Context, userID string) ([]webauthn.Credential, error) {
	rows, err := m.authSvc.store.DB.QueryContext(ctx, `
		SELECT credential_json FROM webauthn_credentials WHERE user_id = ?
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var creds []webauthn.Credential
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var c webauthn.Credential
		if err := json.Unmarshal([]byte(raw), &c); err == nil {
			creds = append(creds, c)
		}
	}
	return creds, rows.Err()
}

func (m *PasskeyManager) ListCredentials(ctx context.Context, userID string) ([]StoredPasskeyCredential, error) {
	rows, err := m.authSvc.store.DB.QueryContext(ctx, `
		SELECT id, name, aaguid, device_type, backed_up, transports_json, sign_count, registered_at, last_used_at
		FROM webauthn_credentials
		WHERE user_id = ?
		ORDER BY registered_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []StoredPasskeyCredential{}
	for rows.Next() {
		var c StoredPasskeyCredential
		var backedUp int
		var transportsJSON string
		var lastUsed sql.NullInt64
		if err := rows.Scan(&c.ID, &c.Name, &c.AAGUID, &c.CredentialDeviceType, &backedUp, &transportsJSON, &c.Counter, &c.RegisteredAt, &lastUsed); err != nil {
			return nil, err
		}
		c.PasskeyUserID = PasskeyUserID(userID)
		c.CredentialBackedUp = backedUp != 0
		_ = json.Unmarshal([]byte(transportsJSON), &c.Transports)
		if c.Transports == nil {
			c.Transports = []string{}
		}
		if lastUsed.Valid {
			v := lastUsed.Int64
			c.LastUsedAt = &v
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (m *PasskeyManager) BeginRegistration(ctx context.Context, u *User, promotion bool) (*protocol.CredentialCreation, string, error) {
	creds, err := m.loadCredentials(ctx, u.ID)
	if err != nil {
		return nil, "", err
	}
	waUser := &webauthnUser{user: u, creds: creds}

	excl := make([]protocol.CredentialDescriptor, 0, len(creds))
	for _, c := range creds {
		excl = append(excl, c.Descriptor())
	}

	sel := protocol.AuthenticatorSelection{
		ResidentKey:        protocol.ResidentKeyRequirementRequired,
		RequireResidentKey: protocol.ResidentKeyRequired(),
		UserVerification:   protocol.VerificationPreferred,
	}
	if promotion {
		sel.AuthenticatorAttachment = protocol.Platform
	}

	opts := []webauthn.RegistrationOption{
		webauthn.WithExclusions(excl),
		webauthn.WithAuthenticatorSelection(sel),
	}
	creation, sess, err := m.wa.BeginRegistration(waUser, opts...)
	if err != nil {
		return nil, "", err
	}
	token := m.storeCeremony(u.ID, sess)
	return creation, token, nil
}

func (m *PasskeyManager) FinishRegistration(ctx context.Context, u *User, ceremonyToken, nickname string, r *http.Request) (*StoredPasskeyCredential, error) {
	pending, ok := m.popCeremony(ceremonyToken)
	if !ok || pending.UserID != u.ID {
		return nil, errors.New("registration session expired; please try again")
	}
	creds, err := m.loadCredentials(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	waUser := &webauthnUser{user: u, creds: creds}

	cred, err := m.wa.FinishRegistration(waUser, *pending.Session, r)
	if err != nil {
		return nil, fmt.Errorf("passkey verification failed: %w", err)
	}

	credID := base64.RawURLEncoding.EncodeToString(cred.ID)
	rawJSON, err := json.Marshal(cred)
	if err != nil {
		return nil, err
	}
	transports := make([]string, 0, len(cred.Transport))
	for _, t := range cred.Transport {
		transports = append(transports, string(t))
	}
	transportsJSON, _ := json.Marshal(transports)

	aaguid := "00000000-0000-0000-0000-000000000000"
	if len(cred.Authenticator.AAGUID) == 16 {
		b := cred.Authenticator.AAGUID
		aaguid = fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	}

	deviceType := "singleDevice"
	if cred.Flags.BackupEligible {
		deviceType = "multiDevice"
	}
	name := strings.TrimSpace(nickname)
	if name == "" {
		name = "Passkey"
	}
	now := time.Now().UnixMilli()

	_, err = m.authSvc.store.DB.ExecContext(ctx, `
		INSERT INTO webauthn_credentials (id, user_id, name, credential_json, aaguid, device_type, backed_up, transports_json, sign_count, registered_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, credID, u.ID, name, string(rawJSON), aaguid, deviceType, boolToInt(cred.Flags.BackupState), string(transportsJSON), cred.Authenticator.SignCount, now)
	if err != nil {
		return nil, err
	}

	return &StoredPasskeyCredential{
		ID:                   credID,
		PasskeyUserID:        PasskeyUserID(u.ID),
		Name:                 name,
		AAGUID:               aaguid,
		CredentialDeviceType: deviceType,
		CredentialBackedUp:   cred.Flags.BackupState,
		Transports:           transports,
		RegisteredAt:         now,
		Counter:              cred.Authenticator.SignCount,
	}, nil
}

func (m *PasskeyManager) BeginDiscoverableLogin() (*protocol.CredentialAssertion, string, error) {
	assertion, sess, err := m.wa.BeginDiscoverableLogin(
		webauthn.WithUserVerification(protocol.VerificationPreferred),
	)
	if err != nil {
		return nil, "", err
	}
	token := m.storeCeremony("", sess)
	return assertion, token, nil
}

var ErrPasskeyNotFound = errors.New("passkey credential not found")

func (m *PasskeyManager) FinishDiscoverableLogin(ctx context.Context, ceremonyToken string, r *http.Request) (*User, error) {
	pending, ok := m.popCeremony(ceremonyToken)
	if !ok {
		return nil, errors.New("passkey sign-in session expired; please try again")
	}

	var matchedUser *User
	var notFound bool
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		credID := base64.RawURLEncoding.EncodeToString(rawID)
		var userID string
		err := m.authSvc.store.DB.QueryRowContext(ctx, `
			SELECT user_id FROM webauthn_credentials WHERE id = ?
		`, credID).Scan(&userID)
		if errors.Is(err, sql.ErrNoRows) {
			notFound = true
			return nil, ErrPasskeyNotFound
		}
		if err != nil {
			return nil, err
		}
		u, err := m.authSvc.GetUserByID(ctx, userID)
		if err != nil {
			notFound = true
			return nil, ErrPasskeyNotFound
		}
		creds, err := m.loadCredentials(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		matchedUser = u
		return &webauthnUser{user: u, creds: creds}, nil
	}

	cred, err := m.wa.FinishDiscoverableLogin(handler, *pending.Session, r)
	if notFound {
		return nil, ErrPasskeyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("passkey assertion failed: %w", err)
	}
	if matchedUser == nil {
		return nil, ErrPasskeyNotFound
	}

	credID := base64.RawURLEncoding.EncodeToString(cred.ID)
	rawJSON, _ := json.Marshal(cred)
	now := time.Now().UnixMilli()
	_, _ = m.authSvc.store.DB.ExecContext(ctx, `
		UPDATE webauthn_credentials SET credential_json = ?, sign_count = ?, last_used_at = ? WHERE id = ?
	`, string(rawJSON), cred.Authenticator.SignCount, now, credID)

	return matchedUser, nil
}

func (m *PasskeyManager) RenameCredential(ctx context.Context, userID, credID, newName string) error {
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return errors.New("passkey name cannot be empty")
	}
	res, err := m.authSvc.store.DB.ExecContext(ctx, `
		UPDATE webauthn_credentials SET name = ? WHERE id = ? AND user_id = ?
	`, newName, credID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (m *PasskeyManager) DeleteCredential(ctx context.Context, userID, credID string) error {
	res, err := m.authSvc.store.DB.ExecContext(ctx, `
		DELETE FROM webauthn_credentials WHERE id = ? AND user_id = ?
	`, credID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

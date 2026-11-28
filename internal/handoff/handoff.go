// Package handoff implements the hub side of the desktop client's browser sign-in.
//
// Passkeys and OIDC are bound to the hub's web origin, so the desktop client cannot run them in
// its own webview. Instead the client opens the system browser on the hub, the user signs in
// there, and the hub hands a short-lived one-time code back to the client through an
// ourchive:// link. The client redeems the code for its own session.
//
// The exchange is protected with PKCE (RFC 7636, S256): the client keeps a random verifier and
// only sends its SHA-256 challenge through the browser, so a code intercepted on the way back
// (another app claiming the ourchive:// scheme, a leaked browser history entry) is useless.
package handoff

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

// DefaultTTL is how long an issued code stays redeemable.
const DefaultTTL = 2 * time.Minute

// maxPending bounds memory if a signed-in user spams the authorize endpoint.
const maxPending = 10000

var (
	// ErrInvalidChallenge means the challenge is not a base64url SHA-256 digest.
	ErrInvalidChallenge = errors.New("invalid code challenge")
	// ErrInvalidState means the client state is missing or malformed.
	ErrInvalidState = errors.New("invalid client state")
	// ErrInvalidCode means the code is unknown, expired or already used.
	ErrInvalidCode = errors.New("invalid or expired sign-in code")
	// ErrVerifierMismatch means the verifier does not hash to the code's challenge.
	ErrVerifierMismatch = errors.New("code verifier does not match")
	// ErrTooManyPending means the store is full.
	ErrTooManyPending = errors.New("too many pending sign-ins; try again shortly")
)

type entry struct {
	userID    string
	challenge string
	expires   time.Time
}

// Store holds pending one-time codes in memory. Codes do not survive a hub restart, which only
// means an in-flight sign-in has to be started again.
type Store struct {
	mu    sync.Mutex
	codes map[string]entry
	ttl   time.Duration
	now   func() time.Time
}

// NewStore returns a store whose codes expire after ttl (DefaultTTL if ttl <= 0).
func NewStore(ttl time.Duration) *Store {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Store{codes: map[string]entry{}, ttl: ttl, now: time.Now}
}

// Challenge returns the S256 challenge for a verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ValidChallenge reports whether c looks like an S256 challenge (43 base64url chars, 32 bytes).
func ValidChallenge(c string) bool {
	if len(c) != 43 {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	return err == nil && len(b) == sha256.Size
}

// ValidState reports whether s is an acceptable opaque client state: 16 to 128 URL-safe chars.
func ValidState(s string) bool {
	if len(s) < 16 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !isUnreserved(r) {
			return false
		}
	}
	return true
}

// ValidVerifier reports whether v satisfies RFC 7636: 43 to 128 unreserved characters.
func ValidVerifier(v string) bool {
	if len(v) < 43 || len(v) > 128 {
		return false
	}
	for _, r := range v {
		if !isUnreserved(r) {
			return false
		}
	}
	return true
}

func isUnreserved(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
		r == '-' || r == '.' || r == '_' || r == '~'
}

// Issue creates a one-time code for userID bound to challenge.
func (s *Store) Issue(userID, challenge string) (string, error) {
	if !ValidChallenge(challenge) {
		return "", ErrInvalidChallenge
	}
	if userID == "" {
		return "", errors.New("user id is required")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	code := base64.RawURLEncoding.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneLocked(now)
	if len(s.codes) >= maxPending {
		return "", ErrTooManyPending
	}
	s.codes[code] = entry{userID: userID, challenge: challenge, expires: now.Add(s.ttl)}
	return code, nil
}

// Redeem consumes code and returns its user if verifier matches. A code is deleted on the first
// redemption attempt whether or not the verifier matches, so it cannot be brute-forced.
func (s *Store) Redeem(code, verifier string) (string, error) {
	s.mu.Lock()
	e, ok := s.codes[code]
	if ok {
		delete(s.codes, code)
	}
	now := s.now()
	s.mu.Unlock()

	if !ok || now.After(e.expires) {
		return "", ErrInvalidCode
	}
	if !ValidVerifier(verifier) {
		return "", ErrVerifierMismatch
	}
	if subtle.ConstantTimeCompare([]byte(Challenge(verifier)), []byte(e.challenge)) != 1 {
		return "", ErrVerifierMismatch
	}
	return e.userID, nil
}

// Pending reports how many unexpired codes are outstanding.
func (s *Store) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(s.now())
	return len(s.codes)
}

func (s *Store) pruneLocked(now time.Time) {
	for c, e := range s.codes {
		if now.After(e.expires) {
			delete(s.codes, c)
		}
	}
}

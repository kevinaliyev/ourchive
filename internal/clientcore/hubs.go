package clientcore

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Hub is a hub the user has saved in the client.
type Hub struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	URL     string    `json:"url"`
	AddedAt time.Time `json:"added_at"`
}

type storedSession struct {
	Token   string    `json:"token"`
	Expires time.Time `json:"expires,omitempty"`
}

type storeFile struct {
	Hubs     []Hub                    `json:"hubs"`
	Active   string                   `json:"active,omitempty"`
	Sessions map[string]storedSession `json:"sessions,omitempty"`
}

// HubStore persists saved hubs, the active hub and one session per hub. Sessions play the role a
// browser's cookie jar would: the webview never sees them, the proxy injects them.
type HubStore struct {
	mu   sync.Mutex
	path string
	now  func() time.Time
	st   storeFile
}

// ErrUnknownHub is returned for an ID that is not saved.
var ErrUnknownHub = errors.New("unknown hub")

// OpenHubStore loads path, or starts empty if it does not exist yet. An empty path keeps
// everything in memory (tests).
func OpenHubStore(path string) (*HubStore, error) {
	s := &HubStore{path: path, now: time.Now, st: storeFile{Sessions: map[string]storedSession{}}}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.st.Sessions == nil {
		s.st.Sessions = map[string]storedSession{}
	}
	return s, nil
}

// NormalizeHubURL validates a user-entered hub address and reduces it to scheme://host[:port].
// A bare host defaults to https.
func NormalizeHubURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("hub address is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid hub address: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", errors.New("hub address must use https:// (or http:// for local testing)")
	}
	if u.Host == "" || u.User != nil {
		return "", errors.New("invalid hub address")
	}
	return scheme + "://" + strings.ToLower(u.Host), nil
}

// hubID derives a stable ID from a normalized hub URL.
func hubID(normalizedURL string) string {
	u, _ := url.Parse(normalizedURL)
	var b strings.Builder
	for _, r := range u.Host {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	id := strings.Trim(b.String(), "-.")
	if u.Scheme == "http" {
		id = "http-" + id
	}
	return id
}

// List returns saved hubs in the order they were added.
func (s *HubStore) List() []Hub {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Hub(nil), s.st.Hubs...)
}

// Get returns a saved hub.
func (s *HubStore) Get(id string) (Hub, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(id)
}

// FindByURL returns the saved hub with this address, if any.
func (s *HubStore) FindByURL(rawURL string) (Hub, bool) {
	norm, err := NormalizeHubURL(rawURL)
	if err != nil {
		return Hub{}, false
	}
	return s.Get(hubID(norm))
}

func (s *HubStore) getLocked(id string) (Hub, bool) {
	for _, h := range s.st.Hubs {
		if h.ID == id {
			return h, true
		}
	}
	return Hub{}, false
}

// Add saves a hub (or returns the existing one with the same address). The first hub saved
// becomes active.
func (s *HubStore) Add(name, rawURL string) (Hub, error) {
	norm, err := NormalizeHubURL(rawURL)
	if err != nil {
		return Hub{}, err
	}
	id := hubID(norm)
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok := s.getLocked(id); ok {
		return h, nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		u, _ := url.Parse(norm)
		name = u.Host
	}
	h := Hub{ID: id, Name: name, URL: norm, AddedAt: s.now().UTC()}
	s.st.Hubs = append(s.st.Hubs, h)
	if s.st.Active == "" {
		s.st.Active = id
	}
	return h, s.saveLocked()
}

// Rename changes a hub's display name, e.g. to the name the hub reports about itself.
func (s *HubStore) Rename(id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.st.Hubs {
		if s.st.Hubs[i].ID == id {
			if s.st.Hubs[i].Name == name {
				return nil
			}
			s.st.Hubs[i].Name = name
			return s.saveLocked()
		}
	}
	return ErrUnknownHub
}

// Remove forgets a hub and its session. If it was active, the next saved hub becomes active.
func (s *HubStore) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i, h := range s.st.Hubs {
		if h.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ErrUnknownHub
	}
	s.st.Hubs = append(s.st.Hubs[:idx], s.st.Hubs[idx+1:]...)
	delete(s.st.Sessions, id)
	if s.st.Active == id {
		s.st.Active = ""
		if len(s.st.Hubs) > 0 {
			s.st.Active = s.st.Hubs[0].ID
		}
	}
	return s.saveLocked()
}

// SetActive selects the hub the UI and proxy talk to.
func (s *HubStore) SetActive(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.getLocked(id); !ok {
		return ErrUnknownHub
	}
	if s.st.Active == id {
		return nil
	}
	s.st.Active = id
	return s.saveLocked()
}

// Active returns the active hub, if any.
func (s *HubStore) Active() (Hub, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(s.st.Active)
}

// Session returns the unexpired session token for a hub.
func (s *HubStore) Session(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.st.Sessions[id]
	if !ok || sess.Token == "" {
		return "", false
	}
	if !sess.Expires.IsZero() && s.now().After(sess.Expires) {
		return "", false
	}
	return sess.Token, true
}

// SetSession stores the session token for a hub. A zero expiry means "until the hub rejects it".
func (s *HubStore) SetSession(id, token string, expires time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.getLocked(id); !ok {
		return ErrUnknownHub
	}
	cur, had := s.st.Sessions[id]
	if had && cur.Token == token && cur.Expires.Equal(expires) {
		return nil
	}
	s.st.Sessions[id] = storedSession{Token: token, Expires: expires.UTC()}
	return s.saveLocked()
}

// ClearSession forgets a hub's session.
func (s *HubStore) ClearSession(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.st.Sessions[id]; !ok {
		return nil
	}
	delete(s.st.Sessions, id)
	return s.saveLocked()
}

// saveLocked writes the store atomically with owner-only permissions.
func (s *HubStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*.json")
	if err != nil {
		return err
	}
	// os.CreateTemp creates the file with mode 0600, so the session tokens stay owner-only.
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

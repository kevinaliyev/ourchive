package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"

	"github.com/ourchive/ourchive/internal/db"
	"github.com/ourchive/ourchive/internal/settings"
)

const (
	RoleHostAdmin  = "host_admin"
	RoleGroupAdmin = "group_admin"
	RoleMember     = "member"

	SessionCookieName = "ourchive_session"
	DefaultSessionTTL = 7 * 24 * time.Hour

	syntheticEmailSuffix = "@local.invalid"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrAlreadyInitialized = errors.New("hub is already initialized")
	ErrUserExists         = errors.New("a user with this email or username already exists")
	ErrNotFound           = errors.New("not found")
	ErrInvalidInvite      = errors.New("invalid, expired, or revoked invite key")
	ErrInviteForbidden    = errors.New("you do not have permission to create invite keys")
	ErrInviteQuotaReached = errors.New("you have reached your invite key quota")

	usernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,30}[a-z0-9]$`)
)

type User struct {
	ID              string `json:"id"`
	Email           string `json:"email"`
	Username        string `json:"username,omitempty"`
	DisplayName     string `json:"display_name"`
	Role            string `json:"role"`
	InvitedBy       string `json:"invited_by,omitempty"`
	InvitedByName   string `json:"invited_by_name,omitempty"`
	InviteKeyID     string `json:"invite_key_id,omitempty"`
	InviteKeyPrefix string `json:"invite_key_prefix,omitempty"`
	CanInvite       bool   `json:"can_invite"`
	InviteQuota     int    `json:"invite_quota"`
	CreatedAt       string `json:"created_at"`
}

// Label returns a human-readable identifier for audit logs.
func (u *User) Label() string {
	if u == nil {
		return "anonymous"
	}
	idPart := u.Username
	if idPart != "" {
		idPart = "@" + idPart
	} else if u.Email != "" {
		idPart = u.Email
	}
	if idPart != "" {
		return fmt.Sprintf("%s (%s)", u.DisplayName, idPart)
	}
	return u.DisplayName
}

type Group struct {
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	Description    string        `json:"description"`
	AuditDownloads bool          `json:"audit_downloads"`
	AuditPreviews  bool          `json:"audit_previews"`
	CreatedAt      string        `json:"created_at"`
	Members        []GroupMember `json:"members"`
}

type GroupMember struct {
	UserID      string `json:"user_id"`
	Email       string `json:"email"`
	Username    string `json:"username,omitempty"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// IdentityProvider defines a pluggable external auth contract (e.g. OIDC) for future phases.
type IdentityProvider interface {
	Name() string
	AuthURL(state string) string
	Exchange(ctx context.Context, code string) (email, displayName string, err error)
}

// PasskeyStore defines a pluggable WebAuthn/Passkey credential store for future phases.
type PasskeyStore interface {
	HasPasskeys(ctx context.Context, userID string) (bool, error)
}

type Service struct {
	store *db.Store
}

func NewService(store *db.Store) *Service {
	return &Service{store: store}
}

func normalizeEmailForOutput(raw string) string {
	if strings.HasSuffix(strings.ToLower(raw), syntheticEmailSuffix) {
		return ""
	}
	return raw
}

func NormalizeUsername(raw string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(raw, "@")))
}

func ValidateIdentity(email, username, mode string) (normEmail, normUsername string, err error) {
	normEmail = strings.ToLower(strings.TrimSpace(email))
	normUsername = NormalizeUsername(username)
	mode = settings.NormalizeIdentityMode(mode)

	if normEmail != "" {
		if !strings.Contains(normEmail, "@") || strings.HasSuffix(normEmail, syntheticEmailSuffix) {
			return "", "", errors.New("valid email address is required")
		}
	}
	if normUsername != "" {
		if !usernameRe.MatchString(normUsername) {
			return "", "", errors.New("username must be 2–32 characters (letters, numbers, ., _, -)")
		}
	}

	switch mode {
	case settings.IdentityEmailOnly:
		if normEmail == "" {
			return "", "", errors.New("email address is required")
		}
	case settings.IdentityUsernameOnly:
		if normUsername == "" {
			return "", "", errors.New("username is required")
		}
	case settings.IdentityBoth:
		if normEmail == "" || normUsername == "" {
			return "", "", errors.New("both email address and username are required")
		}
	default: // email_or_username
		if normEmail == "" && normUsername == "" {
			return "", "", errors.New("an email address or username is required")
		}
	}
	return normEmail, normUsername, nil
}

// HashPassword hashes a password using Argon2id with a random 16-byte salt.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	const (
		argonTime    uint32 = 2
		argonMemory  uint32 = 64 * 1024
		argonThreads uint8  = 4
		argonKeyLen  uint32 = 32
	)
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argonMemory,
		argonTime,
		argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	), nil
}

// VerifyPassword verifies a plaintext password against a PHC Argon2id hash.
func VerifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var memory, iterations uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, iterations, memory, threads, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func (s *Service) UserCount(ctx context.Context) (int, error) {
	var count int
	if err := s.store.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM users`).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Service) SetupHostAdmin(ctx context.Context, email, username, displayName, password string) (*User, error) {
	count, err := s.UserCount(ctx)
	if err != nil {
		return nil, err
	}
	if count > 0 {
		return nil, ErrAlreadyInitialized
	}
	return s.CreateUserWithOptions(ctx, CreateUserOptions{
		Email:        email,
		Username:     username,
		DisplayName:  displayName,
		Password:     password,
		Role:         RoleHostAdmin,
		IdentityMode: settings.IdentityEmailOrUsername,
	})
}

type CreateUserOptions struct {
	Email        string
	Username     string
	DisplayName  string
	Password     string
	Role         string
	IdentityMode string
	InvitedBy    string
	InviteKeyID  string
}

func (s *Service) CreateUser(ctx context.Context, email, displayName, password, role string) (*User, error) {
	return s.CreateUserWithOptions(ctx, CreateUserOptions{
		Email:        email,
		DisplayName:  displayName,
		Password:     password,
		Role:         role,
		IdentityMode: settings.IdentityEmailOrUsername,
	})
}

func (s *Service) CreateUserWithOptions(ctx context.Context, opts CreateUserOptions) (*User, error) {
	normEmail, normUsername, err := ValidateIdentity(opts.Email, opts.Username, opts.IdentityMode)
	if err != nil {
		return nil, err
	}
	displayName := strings.TrimSpace(opts.DisplayName)
	if displayName == "" {
		if normUsername != "" {
			displayName = normUsername
		} else {
			displayName = strings.Split(normEmail, "@")[0]
		}
	}
	if len(opts.Password) < 8 {
		return nil, errors.New("password must be at least 8 characters")
	}
	role := opts.Role
	if role != RoleHostAdmin && role != RoleGroupAdmin && role != RoleMember {
		role = RoleMember
	}

	hash, err := HashPassword(opts.Password)
	if err != nil {
		return nil, err
	}

	userID := uuid.NewString()
	storedEmail := normEmail
	if storedEmail == "" {
		storedEmail = "no-email+" + userID + syntheticEmailSuffix
	}
	var usernameArg any
	if normUsername != "" {
		usernameArg = normUsername
	}
	var invitedByArg any
	if opts.InvitedBy != "" {
		invitedByArg = opts.InvitedBy
	}
	var inviteKeyArg any
	if opts.InviteKeyID != "" {
		inviteKeyArg = opts.InviteKeyID
	}

	nowStr := time.Now().UTC().Format(time.RFC3339)
	_, err = s.store.DB.ExecContext(ctx,
		`INSERT INTO users (id, email, username, display_name, password_hash, role, invited_by, invite_key_id, can_invite, invite_quota, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?)`,
		userID, storedEmail, usernameArg, displayName, hash, role, invitedByArg, inviteKeyArg, nowStr,
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, ErrUserExists
		}
		return nil, err
	}

	return &User{
		ID:          userID,
		Email:       normEmail,
		Username:    normUsername,
		DisplayName: displayName,
		Role:        role,
		InvitedBy:   opts.InvitedBy,
		InviteKeyID: opts.InviteKeyID,
		CreatedAt:   nowStr,
	}, nil
}

// Authenticate verifies a user by either email or username.
func (s *Service) Authenticate(ctx context.Context, identifier, password string) (*User, error) {
	ident := strings.ToLower(strings.TrimSpace(identifier))
	if ident == "" {
		return nil, ErrInvalidCredentials
	}
	uname := strings.TrimPrefix(ident, "@")

	var u User
	var rawEmail string
	var username, invitedBy, inviteKeyID sql.NullString
	var canInvite int
	var hash string
	err := s.store.DB.QueryRowContext(ctx, `
		SELECT id, email, username, display_name, password_hash, role, invited_by, invite_key_id, can_invite, invite_quota, created_at
		FROM users
		WHERE (LOWER(email) = ? AND LOWER(email) NOT LIKE '%@local.invalid')
		   OR (username IS NOT NULL AND LOWER(username) = ?)
		LIMIT 1
	`, ident, uname).Scan(
		&u.ID, &rawEmail, &username, &u.DisplayName, &hash, &u.Role,
		&invitedBy, &inviteKeyID, &canInvite, &u.InviteQuota, &u.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	if !VerifyPassword(password, hash) {
		return nil, ErrInvalidCredentials
	}
	u.Email = normalizeEmailForOutput(rawEmail)
	if username.Valid {
		u.Username = username.String
	}
	if invitedBy.Valid {
		u.InvitedBy = invitedBy.String
	}
	if inviteKeyID.Valid {
		u.InviteKeyID = inviteKeyID.String
	}
	u.CanInvite = canInvite == 1
	return &u, nil
}

func (s *Service) CreateSession(ctx context.Context, userID string, ttl time.Duration) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	sessionID := base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now().UTC()
	expires := now.Add(ttl)
	_, err := s.store.DB.ExecContext(ctx,
		`INSERT INTO sessions (id, user_id, expires_at, created_at) VALUES (?, ?, ?, ?)`,
		sessionID, userID, expires.Format(time.RFC3339), now.Format(time.RFC3339),
	)
	if err != nil {
		return "", time.Time{}, err
	}
	return sessionID, expires, nil
}

func (s *Service) GetSessionUser(ctx context.Context, sessionID string) (*User, error) {
	if sessionID == "" {
		return nil, ErrNotFound
	}
	var u User
	var rawEmail, expiresRaw string
	var username, invitedBy, inviteKeyID sql.NullString
	var canInvite int
	err := s.store.DB.QueryRowContext(ctx, `
		SELECT u.id, u.email, u.username, u.display_name, u.role, u.invited_by, u.invite_key_id, u.can_invite, u.invite_quota, u.created_at, s.expires_at
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.id = ?
	`, sessionID).Scan(
		&u.ID, &rawEmail, &username, &u.DisplayName, &u.Role,
		&invitedBy, &inviteKeyID, &canInvite, &u.InviteQuota, &u.CreatedAt, &expiresRaw,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	expires, err := time.Parse(time.RFC3339, expiresRaw)
	if err != nil || time.Now().UTC().After(expires) {
		_ = s.DeleteSession(ctx, sessionID)
		return nil, ErrNotFound
	}
	u.Email = normalizeEmailForOutput(rawEmail)
	if username.Valid {
		u.Username = username.String
	}
	if invitedBy.Valid {
		u.InvitedBy = invitedBy.String
	}
	if inviteKeyID.Valid {
		u.InviteKeyID = inviteKeyID.String
	}
	u.CanInvite = canInvite == 1
	return &u, nil
}

func (s *Service) DeleteSession(ctx context.Context, sessionID string) error {
	_, err := s.store.DB.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, sessionID)
	return err
}

func (s *Service) DeleteAllSessionsForUser(ctx context.Context, userID string) error {
	_, err := s.store.DB.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// ChangePassword verifies the user's current password, updates to a new Argon2id hash,
// and revokes all existing sessions for that user.
func (s *Service) ChangePassword(ctx context.Context, userID, currentPassword, newPassword string) error {
	if len(newPassword) < 8 {
		return errors.New("new password must be at least 8 characters")
	}
	var currentHash string
	err := s.store.DB.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&currentHash)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !VerifyPassword(currentPassword, currentHash) {
		return ErrInvalidCredentials
	}
	nextHash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	if _, err := s.store.DB.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, nextHash, userID); err != nil {
		return err
	}
	return s.DeleteAllSessionsForUser(ctx, userID)
}

func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.store.DB.QueryContext(ctx, `
		SELECT u.id, u.email, u.username, u.display_name, u.role,
		       u.invited_by, COALESCE(inv.display_name, ''),
		       u.invite_key_id, COALESCE(ik.code_prefix, ''),
		       u.can_invite, u.invite_quota, u.created_at
		FROM users u
		LEFT JOIN users inv ON inv.id = u.invited_by
		LEFT JOIN invite_keys ik ON ik.id = u.invite_key_id
		ORDER BY u.created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := []User{}
	for rows.Next() {
		var u User
		var rawEmail string
		var username, invitedBy, invitedByName, inviteKeyID, inviteKeyPrefix sql.NullString
		var canInvite int
		if err := rows.Scan(
			&u.ID, &rawEmail, &username, &u.DisplayName, &u.Role,
			&invitedBy, &invitedByName, &inviteKeyID, &inviteKeyPrefix,
			&canInvite, &u.InviteQuota, &u.CreatedAt,
		); err != nil {
			return nil, err
		}
		u.Email = normalizeEmailForOutput(rawEmail)
		if username.Valid {
			u.Username = username.String
		}
		if invitedBy.Valid {
			u.InvitedBy = invitedBy.String
		}
		if invitedByName.Valid {
			u.InvitedByName = invitedByName.String
		}
		if inviteKeyID.Valid {
			u.InviteKeyID = inviteKeyID.String
		}
		if inviteKeyPrefix.Valid {
			u.InviteKeyPrefix = inviteKeyPrefix.String
		}
		u.CanInvite = canInvite == 1
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *Service) UpdateUserInvitePerms(ctx context.Context, userID string, canInvite bool, inviteQuota int) error {
	if inviteQuota < 0 {
		inviteQuota = 0
	}
	ci := 0
	if canInvite {
		ci = 1
	}
	res, err := s.store.DB.ExecContext(ctx, `UPDATE users SET can_invite = ?, invite_quota = ? WHERE id = ?`, ci, inviteQuota, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) DeleteUser(ctx context.Context, userID string) error {
	res, err := s.store.DB.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := s.store.DB.QueryContext(ctx, `SELECT id, name, description, audit_downloads, audit_previews, created_at FROM groups ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	groups := []Group{}
	byID := map[string]int{}
	for rows.Next() {
		var g Group
		var auditDl, auditPv int
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &auditDl, &auditPv, &g.CreatedAt); err != nil {
			return nil, err
		}
		g.AuditDownloads = auditDl == 1
		g.AuditPreviews = auditPv == 1
		g.Members = []GroupMember{}
		byID[g.ID] = len(groups)
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	mRows, err := s.store.DB.QueryContext(ctx, `
		SELECT gm.group_id, gm.user_id, u.email, u.username, u.display_name, gm.role
		FROM group_members gm
		JOIN users u ON u.id = gm.user_id
		ORDER BY u.display_name ASC
	`)
	if err != nil {
		return nil, err
	}
	defer mRows.Close()

	for mRows.Next() {
		var gid, rawEmail string
		var uname sql.NullString
		var m GroupMember
		if err := mRows.Scan(&gid, &m.UserID, &rawEmail, &uname, &m.DisplayName, &m.Role); err != nil {
			return nil, err
		}
		m.Email = normalizeEmailForOutput(rawEmail)
		if uname.Valid {
			m.Username = uname.String
		}
		if idx, ok := byID[gid]; ok {
			groups[idx].Members = append(groups[idx].Members, m)
		}
	}
	return groups, mRows.Err()
}

func (s *Service) CreateGroup(ctx context.Context, name, description string) (*Group, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("group name is required")
	}
	g := &Group{
		ID:          uuid.NewString(),
		Name:        name,
		Description: strings.TrimSpace(description),
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		Members:     []GroupMember{},
	}
	_, err := s.store.DB.ExecContext(ctx,
		`INSERT INTO groups (id, name, description, audit_downloads, audit_previews, created_at) VALUES (?, ?, ?, 0, 0, ?)`,
		g.ID, g.Name, g.Description, g.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return g, nil
}

func (s *Service) DeleteGroup(ctx context.Context, groupID string) error {
	_, err := s.store.DB.ExecContext(ctx, `DELETE FROM groups WHERE id = ?`, groupID)
	return err
}

func (s *Service) AddGroupMember(ctx context.Context, groupID, userID, role string) error {
	if role != "admin" {
		role = "member"
	}
	_, err := s.store.DB.ExecContext(ctx, `
		INSERT INTO group_members (group_id, user_id, role) VALUES (?, ?, ?)
		ON CONFLICT(group_id, user_id) DO UPDATE SET role = excluded.role
	`, groupID, userID, role)
	return err
}

func (s *Service) RemoveGroupMember(ctx context.Context, groupID, userID string) error {
	_, err := s.store.DB.ExecContext(ctx, `DELETE FROM group_members WHERE group_id = ? AND user_id = ?`, groupID, userID)
	return err
}

// IsGroupAdminOf returns true if the user is a host_admin or an admin member of the given group (or global group_admin in that group).
func (s *Service) IsGroupAdminOf(ctx context.Context, u *User, groupID string) bool {
	if u == nil {
		return false
	}
	if u.Role == RoleHostAdmin {
		return true
	}
	var role string
	err := s.store.DB.QueryRowContext(ctx, `SELECT role FROM group_members WHERE group_id = ? AND user_id = ?`, groupID, u.ID).Scan(&role)
	if err != nil {
		return false
	}
	return role == "admin" || u.Role == RoleGroupAdmin
}

// UpdateGroupAuditSettings updates a group's download/preview audit toggles.
// Host Admin global settings supersede group toggles: if off globally, enabling for a group is rejected.
func (s *Service) UpdateGroupAuditSettings(ctx context.Context, groupID string, auditDownloads, auditPreviews, globalDownloads, globalPreviews bool) error {
	if auditDownloads && !globalDownloads {
		return errors.New("file download auditing is disabled globally by the Host Admin")
	}
	if auditPreviews && !globalPreviews {
		return errors.New("file preview auditing is disabled globally by the Host Admin")
	}
	dl, pv := 0, 0
	if auditDownloads {
		dl = 1
	}
	if auditPreviews {
		pv = 1
	}
	res, err := s.store.DB.ExecContext(ctx, `UPDATE groups SET audit_downloads = ?, audit_previews = ? WHERE id = ?`, dl, pv, groupID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- HTTP Cookie & Context Helpers ---

type ctxKey struct{}

func WithUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

func UserFromContext(ctx context.Context) *User {
	u, _ := ctx.Value(ctxKey{}).(*User)
	return u
}

func SetSessionCookie(w http.ResponseWriter, r *http.Request, sessionID string, expires time.Time) {
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		Expires:  expires,
	})
}

func ClearSessionCookie(w http.ResponseWriter, r *http.Request) {
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   -1,
	})
}

func (s *Service) CurrentUser(r *http.Request) *User {
	c, err := r.Cookie(SessionCookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	u, err := s.GetSessionUser(r.Context(), c.Value)
	if err != nil {
		return nil
	}
	return u
}

func (s *Service) RequireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := s.CurrentUser(r)
		if u == nil {
			http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
			return
		}
		next(w, r.WithContext(WithUser(r.Context(), u)))
	}
}

func (s *Service) RequireRoles(roles []string, next http.HandlerFunc) http.HandlerFunc {
	return s.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := UserFromContext(r.Context())
		for _, allowed := range roles {
			if u.Role == allowed {
				next(w, r)
				return
			}
		}
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
	})
}

func (s *Service) GetUserByID(ctx context.Context, userID string) (*User, error) {
	var u User
	var email, username, invitedBy, inviteKeyID sql.NullString
	var canInvite int
	err := s.store.DB.QueryRowContext(ctx, `
		SELECT id, email, COALESCE(username, ''), display_name, role,
		       COALESCE(invited_by, ''), COALESCE(invite_key_id, ''),
		       COALESCE(can_invite, 0), COALESCE(invite_quota, 0), created_at
		FROM users WHERE id = ?
	`, userID).Scan(&u.ID, &email, &username, &u.DisplayName, &u.Role, &invitedBy, &inviteKeyID, &canInvite, &u.InviteQuota, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.Email = normalizeEmailForOutput(email.String)
	u.Username = username.String
	u.InvitedBy = invitedBy.String
	u.InviteKeyID = inviteKeyID.String
	u.CanInvite = canInvite != 0
	return &u, nil
}

func (s *Service) UpdateProfile(ctx context.Context, userID, username, displayName string) (*User, error) {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return nil, errors.New("display name cannot be empty")
	}
	normUser := NormalizeUsername(username)
	if normUser != "" && !usernameRe.MatchString(normUser) {
		return nil, errors.New("username must be 2–32 characters (letters, numbers, ., _, -)")
	}
	var userVal any = nil
	if normUser != "" {
		userVal = normUser
	}
	_, err := s.store.DB.ExecContext(ctx, `
		UPDATE users SET username = ?, display_name = ? WHERE id = ?
	`, userVal, displayName, userID)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, ErrUserExists
		}
		return nil, err
	}
	return s.GetUserByID(ctx, userID)
}

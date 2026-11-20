package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

const inviteAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

type InviteKey struct {
	ID          string        `json:"id"`
	CodePrefix  string        `json:"code_prefix"`
	Code        string        `json:"code,omitempty"` // Only populated immediately after creation
	CreatedBy   string        `json:"created_by"`
	CreatorName string        `json:"creator_name"`
	Role        string        `json:"role"`
	GroupID     string        `json:"group_id,omitempty"`
	GroupName   string        `json:"group_name,omitempty"`
	MaxUses     int           `json:"max_uses"`
	UseCount    int           `json:"use_count"`
	Note        string        `json:"note"`
	ExpiresAt   string        `json:"expires_at,omitempty"`
	RevokedAt   string        `json:"revoked_at,omitempty"`
	Expired     bool          `json:"expired"`
	Revoked     bool          `json:"revoked"`
	CreatedAt   string        `json:"created_at"`
	RedeemedBy  []InvitedUser `json:"redeemed_by"`
}

type InvitedUser struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Username    string `json:"username,omitempty"`
	Email       string `json:"email,omitempty"`
	CreatedAt   string `json:"created_at"`
}

type CreateInviteRequest struct {
	Role           string `json:"role"`
	GroupID        string `json:"group_id"`
	MaxUses        int    `json:"max_uses"`
	ExpiresInHours int    `json:"expires_in_hours"`
	Note           string `json:"note"`
}

// CanUserCreateInvite returns true if the user is permitted to mint invite keys under current policy.
func CanUserCreateInvite(u *User, allowGroupAdminInvites bool) bool {
	if u == nil {
		return false
	}
	if u.Role == RoleHostAdmin {
		return true
	}
	if u.Role == RoleGroupAdmin && allowGroupAdminInvites {
		return true
	}
	return u.CanInvite
}

func generateInviteCode() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	chars := make([]byte, 12)
	for i, b := range raw {
		chars[i] = inviteAlphabet[int(b)%len(inviteAlphabet)]
	}
	return "OUR-" + string(chars[0:4]) + "-" + string(chars[4:8]) + "-" + string(chars[8:12]), nil
}

func normalizeInviteCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

func hashInviteCode(code string) string {
	sum := sha256.Sum256([]byte(normalizeInviteCode(code)))
	return hex.EncodeToString(sum[:])
}

func (s *Service) CreateInviteKey(ctx context.Context, actor *User, req CreateInviteRequest, allowGroupAdminInvites bool) (*InviteKey, error) {
	if !CanUserCreateInvite(actor, allowGroupAdminInvites) {
		return nil, ErrInviteForbidden
	}

	// Non-host-admins with a positive invite quota are capped on non-revoked keys minted.
	if actor.Role != RoleHostAdmin && actor.InviteQuota > 0 {
		var minted int
		if err := s.store.DB.QueryRowContext(ctx,
			`SELECT COUNT(1) FROM invite_keys WHERE created_by = ? AND revoked_at IS NULL`,
			actor.ID,
		).Scan(&minted); err != nil {
			return nil, err
		}
		if minted >= actor.InviteQuota {
			return nil, ErrInviteQuotaReached
		}
	}

	role := RoleMember
	if actor.Role == RoleHostAdmin {
		if req.Role == RoleGroupAdmin || req.Role == RoleHostAdmin || req.Role == RoleMember {
			role = req.Role
		}
	}

	maxUses := req.MaxUses
	if maxUses <= 0 {
		maxUses = 1
	}
	if actor.Role != RoleHostAdmin && maxUses > 5 {
		maxUses = 5
	}

	code, err := generateInviteCode()
	if err != nil {
		return nil, err
	}
	prefix := code[:8] // e.g. OUR-7K2M
	codeHash := hashInviteCode(code)

	now := time.Now().UTC()
	var expiresArg any
	var expiresStr string
	if req.ExpiresInHours > 0 {
		expiresStr = now.Add(time.Duration(req.ExpiresInHours) * time.Hour).Format(time.RFC3339)
		expiresArg = expiresStr
	}

	var groupArg any
	groupID := strings.TrimSpace(req.GroupID)
	if groupID != "" {
		groupArg = groupID
	}

	id := uuid.NewString()
	nowStr := now.Format(time.RFC3339)
	note := strings.TrimSpace(req.Note)

	_, err = s.store.DB.ExecContext(ctx, `
		INSERT INTO invite_keys (id, code_prefix, code_hash, created_by, role, group_id, max_uses, use_count, note, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?)
	`, id, prefix, codeHash, actor.ID, role, groupArg, maxUses, note, expiresArg, nowStr)
	if err != nil {
		return nil, err
	}

	return &InviteKey{
		ID:          id,
		CodePrefix:  prefix,
		Code:        code,
		CreatedBy:   actor.ID,
		CreatorName: actor.DisplayName,
		Role:        role,
		GroupID:     groupID,
		MaxUses:     maxUses,
		UseCount:    0,
		Note:        note,
		ExpiresAt:   expiresStr,
		CreatedAt:   nowStr,
		RedeemedBy:  []InvitedUser{},
	}, nil
}

func (s *Service) ListInviteKeys(ctx context.Context, viewer *User) ([]InviteKey, error) {
	query := `
		SELECT ik.id, ik.code_prefix, ik.created_by, COALESCE(u.display_name, ''),
		       ik.role, ik.group_id, COALESCE(g.name, ''),
		       ik.max_uses, ik.use_count, ik.note, ik.expires_at, ik.revoked_at, ik.created_at
		FROM invite_keys ik
		LEFT JOIN users u ON u.id = ik.created_by
		LEFT JOIN groups g ON g.id = ik.group_id
	`
	args := []any{}
	if viewer.Role != RoleHostAdmin {
		query += ` WHERE ik.created_by = ?`
		args = append(args, viewer.ID)
	}
	query += ` ORDER BY ik.created_at DESC`

	rows, err := s.store.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	now := time.Now().UTC()
	keys := []InviteKey{}
	byID := map[string]int{}
	for rows.Next() {
		var k InviteKey
		var groupID, expiresAt, revokedAt sql.NullString
		if err := rows.Scan(
			&k.ID, &k.CodePrefix, &k.CreatedBy, &k.CreatorName,
			&k.Role, &groupID, &k.GroupName,
			&k.MaxUses, &k.UseCount, &k.Note, &expiresAt, &revokedAt, &k.CreatedAt,
		); err != nil {
			return nil, err
		}
		if groupID.Valid {
			k.GroupID = groupID.String
		}
		if expiresAt.Valid && expiresAt.String != "" {
			k.ExpiresAt = expiresAt.String
			if t, err := time.Parse(time.RFC3339, expiresAt.String); err == nil && now.After(t) {
				k.Expired = true
			}
		}
		if revokedAt.Valid && revokedAt.String != "" {
			k.RevokedAt = revokedAt.String
			k.Revoked = true
		}
		k.RedeemedBy = []InvitedUser{}
		byID[k.ID] = len(keys)
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	// Attach redeemed users for lineage visibility.
	uRows, err := s.store.DB.QueryContext(ctx, `
		SELECT id, display_name, username, email, invite_key_id, created_at
		FROM users
		WHERE invite_key_id IS NOT NULL AND invite_key_id != ''
		ORDER BY created_at ASC
	`)
	if err != nil {
		return keys, nil
	}
	defer uRows.Close()
	for uRows.Next() {
		var iu InvitedUser
		var uname sql.NullString
		var rawEmail, keyID string
		if err := uRows.Scan(&iu.ID, &iu.DisplayName, &uname, &rawEmail, &keyID, &iu.CreatedAt); err != nil {
			continue
		}
		iu.Email = normalizeEmailForOutput(rawEmail)
		if uname.Valid {
			iu.Username = uname.String
		}
		if idx, ok := byID[keyID]; ok {
			keys[idx].RedeemedBy = append(keys[idx].RedeemedBy, iu)
		}
	}

	return keys, nil
}

func (s *Service) RevokeInviteKey(ctx context.Context, viewer *User, keyID string) error {
	nowStr := time.Now().UTC().Format(time.RFC3339)
	var res sql.Result
	var err error
	if viewer.Role == RoleHostAdmin {
		res, err = s.store.DB.ExecContext(ctx, `UPDATE invite_keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, nowStr, keyID)
	} else {
		res, err = s.store.DB.ExecContext(ctx, `UPDATE invite_keys SET revoked_at = ? WHERE id = ? AND created_by = ? AND revoked_at IS NULL`, nowStr, keyID, viewer.ID)
	}
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RedeemInviteAndRegister validates an invite key, atomically consumes one use, creates the user
// with invited_by & invite_key_id lineage, and optionally adds them to the invite's group.
func (s *Service) RedeemInviteAndRegister(ctx context.Context, code, email, username, displayName, password, identityMode string) (*User, string, error) {
	normCode := normalizeInviteCode(code)
	if normCode == "" {
		return nil, "", ErrInvalidInvite
	}
	normEmail, normUsername, err := ValidateIdentity(email, username, identityMode)
	if err != nil {
		return nil, "", err
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		if normUsername != "" {
			displayName = normUsername
		} else {
			displayName = strings.Split(normEmail, "@")[0]
		}
	}
	if len(password) < 8 {
		return nil, "", errors.New("password must be at least 8 characters")
	}

	hash, err := HashPassword(password)
	if err != nil {
		return nil, "", err
	}

	tx, err := s.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback() }()

	var keyID, codePrefix, createdBy, role string
	var groupID, expiresAt, revokedAt sql.NullString
	var maxUses, useCount int
	err = tx.QueryRowContext(ctx, `
		SELECT id, code_prefix, created_by, role, group_id, max_uses, use_count, expires_at, revoked_at
		FROM invite_keys
		WHERE code_hash = ?
	`, hashInviteCode(normCode)).Scan(
		&keyID, &codePrefix, &createdBy, &role, &groupID, &maxUses, &useCount, &expiresAt, &revokedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrInvalidInvite
	}
	if err != nil {
		return nil, "", err
	}
	if revokedAt.Valid && revokedAt.String != "" {
		return nil, "", ErrInvalidInvite
	}
	if expiresAt.Valid && expiresAt.String != "" {
		if t, perr := time.Parse(time.RFC3339, expiresAt.String); perr == nil && time.Now().UTC().After(t) {
			return nil, "", ErrInvalidInvite
		}
	}
	if maxUses > 0 && useCount >= maxUses {
		return nil, "", ErrInvalidInvite
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
	nowStr := time.Now().UTC().Format(time.RFC3339)

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO users (id, email, username, display_name, password_hash, role, invited_by, invite_key_id, can_invite, invite_quota, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?)
	`, userID, storedEmail, usernameArg, displayName, hash, role, createdBy, keyID, nowStr); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, "", ErrUserExists
		}
		return nil, "", err
	}

	if _, err := tx.ExecContext(ctx, `UPDATE invite_keys SET use_count = use_count + 1 WHERE id = ?`, keyID); err != nil {
		return nil, "", err
	}

	if groupID.Valid && groupID.String != "" {
		_, _ = tx.ExecContext(ctx, `
			INSERT INTO group_members (group_id, user_id, role) VALUES (?, ?, 'member')
			ON CONFLICT(group_id, user_id) DO NOTHING
		`, groupID.String, userID)
	}

	if err := tx.Commit(); err != nil {
		return nil, "", err
	}

	return &User{
		ID:              userID,
		Email:           normEmail,
		Username:        normUsername,
		DisplayName:     displayName,
		Role:            role,
		InvitedBy:       createdBy,
		InviteKeyID:     keyID,
		InviteKeyPrefix: codePrefix,
		CreatedAt:       nowStr,
	}, codePrefix, nil
}

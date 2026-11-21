// Package auth — Per-mount group ACL persistence and scope enforcement.
// Mounts with zero ACL rows inherit default member read/share visibility;
// mounts with ACL rows restrict non-owner access to matching group scopes.
package auth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ShareUserRule sets a per-user access level on a machine (MountID == "") or
// a specific folder/file mount (MountID != "").
// Access levels:
//   - "none":     completely hidden / blocked (no list, view, download, or share)
//   - "view":     can browse & inline preview, cannot download / zip / create share links
//   - "download": can browse & download / zip, cannot inline preview
//   - "all":      full read + view + download + share access
type ShareUserRule struct {
	ID          string `json:"id"`
	NodeID      string `json:"node_id"`
	MountID     string `json:"mount_id"`
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name,omitempty"`
	Username    string `json:"username,omitempty"`
	Access      string `json:"access"`
	CreatedAt   string `json:"created_at"`
}

// MemberSummary is a minimal non-admin hub user directory entry for sharing rules.
type MemberSummary struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Username    string `json:"username,omitempty"`
}

// MountAccess represents the resolved effective permissions for a user on a mount.
type MountAccess struct {
	Level       string // "none" | "view" | "download" | "all"
	CanList     bool
	CanView     bool
	CanDownload bool
	CanShare    bool
}

func validShareAccess(a string) bool {
	switch a {
	case "none", "view", "download", "all":
		return true
	default:
		return false
	}
}

func accessFromLevel(level string) MountAccess {
	switch level {
	case "view":
		return MountAccess{Level: "view", CanList: true, CanView: true, CanDownload: false, CanShare: false}
	case "download":
		return MountAccess{Level: "download", CanList: true, CanView: false, CanDownload: true, CanShare: true}
	case "all":
		return MountAccess{Level: "all", CanList: true, CanView: true, CanDownload: true, CanShare: true}
	default:
		return MountAccess{Level: "none"}
	}
}

// ListMemberDirectory returns minimal hub member identities so any contributor
// can select users when configuring share access rules.
func (s *Service) ListMemberDirectory(ctx context.Context) ([]MemberSummary, error) {
	rows, err := s.store.DB.QueryContext(ctx, `
		SELECT id, display_name, COALESCE(username, '')
		FROM users
		ORDER BY LOWER(display_name) ASC, created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []MemberSummary{}
	for rows.Next() {
		var m MemberSummary
		if err := rows.Scan(&m.ID, &m.DisplayName, &m.Username); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Service) ListShareUserRules(ctx context.Context, nodeID, mountID string, filterMount bool) ([]ShareUserRule, error) {
	query := `
		SELECT r.id, r.node_id, r.mount_id, r.user_id,
		       COALESCE(u.display_name, ''), COALESCE(u.username, ''),
		       r.access, r.created_at
		FROM share_user_rules r
		LEFT JOIN users u ON u.id = r.user_id
		WHERE r.node_id = ?
	`
	args := []any{nodeID}
	if filterMount {
		query += ` AND r.mount_id = ?`
		args = append(args, mountID)
	}
	query += ` ORDER BY r.mount_id ASC, LOWER(COALESCE(u.display_name, '')) ASC`

	rows, err := s.store.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ShareUserRule{}
	for rows.Next() {
		var r ShareUserRule
		if err := rows.Scan(&r.ID, &r.NodeID, &r.MountID, &r.UserID, &r.DisplayName, &r.Username, &r.Access, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Service) UpsertShareUserRule(ctx context.Context, r ShareUserRule) (*ShareUserRule, error) {
	r.NodeID = strings.TrimSpace(r.NodeID)
	r.MountID = strings.TrimSpace(r.MountID)
	r.UserID = strings.TrimSpace(r.UserID)
	r.Access = strings.ToLower(strings.TrimSpace(r.Access))
	if r.NodeID == "" || r.UserID == "" {
		return nil, errors.New("node_id and user_id are required")
	}
	if !validShareAccess(r.Access) {
		return nil, errors.New("access must be one of: none, view, download, all")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	id := uuid.NewString()
	_, err := s.store.DB.ExecContext(ctx, `
		INSERT INTO share_user_rules (id, node_id, mount_id, user_id, access, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(node_id, mount_id, user_id) DO UPDATE SET
			access = excluded.access
	`, id, r.NodeID, r.MountID, r.UserID, r.Access, now)
	if err != nil {
		return nil, err
	}
	list, err := s.ListShareUserRules(ctx, r.NodeID, r.MountID, true)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].UserID == r.UserID {
			return &list[i], nil
		}
	}
	return &r, nil
}

func (s *Service) GetShareUserRuleNodeID(ctx context.Context, id string) (string, error) {
	var nodeID string
	err := s.store.DB.QueryRowContext(ctx, `SELECT node_id FROM share_user_rules WHERE id = ?`, id).Scan(&nodeID)
	if err != nil {
		return "", ErrNotFound
	}
	return nodeID, nil
}

func (s *Service) DeleteShareUserRule(ctx context.Context, id string) error {
	res, err := s.store.DB.ExecContext(ctx, `DELETE FROM share_user_rules WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// IsUserRestrictedFromNode returns true if the node is private or the user has
// a machine-wide "none" rule on it.
func (s *Service) IsUserRestrictedFromNode(ctx context.Context, userID, nodeID string) bool {
	var access string
	if err := s.store.DB.QueryRowContext(ctx,
		`SELECT access FROM share_user_rules WHERE node_id = ? AND mount_id = '' AND user_id = ?`,
		nodeID, userID,
	).Scan(&access); err == nil {
		return access == "none"
	}
	var vis string
	if err := s.store.DB.QueryRowContext(ctx, `SELECT visibility FROM nodes WHERE id = ?`, nodeID).Scan(&vis); err == nil {
		return vis == "private"
	}
	return false
}

// MountACL grants a specific group fine-grained scopes on a node's mount.
// When a mount has zero ACL rows, default hub visibility applies (all authenticated
// members can read/share; only the node owner can write/delete).
// When a mount has one or more ACL rows, non-owner users must belong to a group
// with the required scope enabled.
type MountACL struct {
	ID         string `json:"id"`
	NodeID     string `json:"node_id"`
	MountID    string `json:"mount_id"`
	GroupID    string `json:"group_id"`
	GroupName  string `json:"group_name,omitempty"`
	CanRead    bool   `json:"can_read"`
	CanWrite   bool   `json:"can_write"`
	CanDelete  bool   `json:"can_delete"`
	CanShare   bool   `json:"can_share"`
	CanCompute bool   `json:"can_compute"`
	CreatedAt  string `json:"created_at"`
}

func (s *Service) ListMountACLs(ctx context.Context, nodeID, mountID string) ([]MountACL, error) {
	query := `
		SELECT a.id, a.node_id, a.mount_id, a.group_id, COALESCE(g.name, ''),
		       a.can_read, a.can_write, a.can_delete, a.can_share, a.can_compute, a.created_at
		FROM mount_group_acls a
		LEFT JOIN groups g ON g.id = a.group_id
		WHERE 1=1
	`
	args := []any{}
	if nodeID != "" {
		query += ` AND a.node_id = ?`
		args = append(args, nodeID)
	}
	if mountID != "" {
		query += ` AND a.mount_id = ?`
		args = append(args, mountID)
	}
	query += ` ORDER BY a.node_id, a.mount_id, g.name`

	rows, err := s.store.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []MountACL{}
	for rows.Next() {
		var a MountACL
		var r, w, d, sh, c int
		if err := rows.Scan(&a.ID, &a.NodeID, &a.MountID, &a.GroupID, &a.GroupName, &r, &w, &d, &sh, &c, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.CanRead = r != 0
		a.CanWrite = w != 0
		a.CanDelete = d != 0
		a.CanShare = sh != 0
		a.CanCompute = c != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Service) UpsertMountACL(ctx context.Context, a MountACL) (*MountACL, error) {
	if a.NodeID == "" || a.MountID == "" || a.GroupID == "" {
		return nil, errors.New("node_id, mount_id, and group_id are required")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	id := uuid.NewString()
	_, err := s.store.DB.ExecContext(ctx, `
		INSERT INTO mount_group_acls (id, node_id, mount_id, group_id, can_read, can_write, can_delete, can_share, can_compute, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(node_id, mount_id, group_id) DO UPDATE SET
			can_read = excluded.can_read,
			can_write = excluded.can_write,
			can_delete = excluded.can_delete,
			can_share = excluded.can_share,
			can_compute = excluded.can_compute
	`, id, a.NodeID, a.MountID, a.GroupID, boolToInt(a.CanRead), boolToInt(a.CanWrite), boolToInt(a.CanDelete), boolToInt(a.CanShare), boolToInt(a.CanCompute), now)
	if err != nil {
		return nil, err
	}
	list, err := s.ListMountACLs(ctx, a.NodeID, a.MountID)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].GroupID == a.GroupID {
			return &list[i], nil
		}
	}
	return &a, nil
}

func (s *Service) GetMountACLNodeID(ctx context.Context, id string) (string, error) {
	var nodeID string
	err := s.store.DB.QueryRowContext(ctx, `SELECT node_id FROM mount_group_acls WHERE id = ?`, id).Scan(&nodeID)
	if err != nil {
		return "", ErrNotFound
	}
	return nodeID, nil
}

func (s *Service) GetMountNodeID(ctx context.Context, mountID string) (string, error) {
	var nodeID string
	err := s.store.DB.QueryRowContext(ctx, `SELECT node_id FROM mounts WHERE id = ?`, mountID).Scan(&nodeID)
	if err != nil {
		return "", ErrNotFound
	}
	return nodeID, nil
}

func (s *Service) DeleteMountACL(ctx context.Context, id string) error {
	res, err := s.store.DB.ExecContext(ctx, `DELETE FROM mount_group_acls WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// EffectiveMountAccess resolves the non-owner user's effective access on (nodeID, mountID),
// combining per-folder user rules, per-folder visibility, machine-wide user rules,
// machine-wide visibility, and legacy per-mount group ACLs.
func (s *Service) EffectiveMountAccess(ctx context.Context, userID, nodeID, mountID string) (MountAccess, error) {
	// 1. Per-folder user rule takes highest precedence.
	var userAccess string
	err := s.store.DB.QueryRowContext(ctx, `
		SELECT access FROM share_user_rules
		WHERE node_id = ? AND mount_id = ? AND user_id = ?
	`, nodeID, mountID, userID).Scan(&userAccess)
	if err == nil {
		return accessFromLevel(userAccess), nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return MountAccess{}, err
	}

	// Load folder & machine visibility (defaulting gracefully if rows don't exist in unit tests).
	mountVis := "inherit"
	if err := s.store.DB.QueryRowContext(ctx, `SELECT visibility FROM mounts WHERE id = ?`, mountID).Scan(&mountVis); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return MountAccess{}, err
	}
	if mountVis == "" {
		mountVis = "inherit"
	}
	nodeVis := "open"
	if err := s.store.DB.QueryRowContext(ctx, `SELECT visibility FROM nodes WHERE id = ?`, nodeID).Scan(&nodeVis); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return MountAccess{}, err
	}
	if nodeVis == "" {
		nodeVis = "open"
	}

	// Helper to check legacy group ACLs on this mount.
	checkGroupACL := func() (hasAny bool, canRead bool, canShare bool, err error) {
		var total int
		if err = s.store.DB.QueryRowContext(ctx, `
			SELECT COUNT(1) FROM mount_group_acls WHERE node_id = ? AND mount_id = ?
		`, nodeID, mountID).Scan(&total); err != nil {
			return false, false, false, err
		}
		if total == 0 {
			return false, false, false, nil
		}
		var rCnt, shCnt int
		if err = s.store.DB.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(CASE WHEN a.can_read = 1 THEN 1 ELSE 0 END), 0),
			       COALESCE(SUM(CASE WHEN a.can_share = 1 THEN 1 ELSE 0 END), 0)
			FROM mount_group_acls a
			INNER JOIN group_members gm ON gm.group_id = a.group_id
			WHERE a.node_id = ? AND a.mount_id = ? AND gm.user_id = ?
		`, nodeID, mountID, userID).Scan(&rCnt, &shCnt); err != nil {
			return true, false, false, err
		}
		return true, rCnt > 0, shCnt > 0, nil
	}

	// 2. If folder explicitly set to private, only explicit folder user rules or group ACLs grant access.
	if mountVis == "private" {
		hasAny, canRead, canShare, err := checkGroupACL()
		if err != nil {
			return MountAccess{}, err
		}
		if hasAny && canRead {
			return MountAccess{Level: "all", CanList: true, CanView: true, CanDownload: true, CanShare: canShare}, nil
		}
		return accessFromLevel("none"), nil
	}

	// 3. Machine-wide user rule applies when folder doesn't override for this user.
	err = s.store.DB.QueryRowContext(ctx, `
		SELECT access FROM share_user_rules
		WHERE node_id = ? AND mount_id = '' AND user_id = ?
	`, nodeID, userID).Scan(&userAccess)
	if err == nil {
		return accessFromLevel(userAccess), nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return MountAccess{}, err
	}

	// 4. Effective visibility (folder override or inherited from machine).
	effVis := mountVis
	if effVis == "inherit" {
		effVis = nodeVis
	}
	hasAny, canRead, canShare, err := checkGroupACL()
	if err != nil {
		return MountAccess{}, err
	}
	if effVis == "private" {
		if hasAny && canRead {
			return MountAccess{Level: "all", CanList: true, CanView: true, CanDownload: true, CanShare: canShare}, nil
		}
		return accessFromLevel("none"), nil
	}
	if hasAny {
		if canRead {
			return MountAccess{Level: "all", CanList: true, CanView: true, CanDownload: true, CanShare: canShare}, nil
		}
		return accessFromLevel("none"), nil
	}
	return accessFromLevel("all"), nil
}

// CheckMountScope evaluates whether a non-owner user has the requested scope
// ("read" | "view" | "download" | "share" | "write" | "delete" | "compute") on a node's mount.
// Node owners always have full access and should bypass this check.
func (s *Service) CheckMountScope(ctx context.Context, userID, nodeID, mountID, scope string) (bool, error) {
	acc, err := s.EffectiveMountAccess(ctx, userID, nodeID, mountID)
	if err != nil {
		return false, err
	}
	switch scope {
	case "read":
		return acc.CanList, nil
	case "view":
		return acc.CanView, nil
	case "download":
		return acc.CanDownload, nil
	case "share":
		return acc.CanShare, nil
	}

	// Elevated scopes ("write" | "delete" | "compute") require both non-blocked access and an explicit group ACL.
	if !acc.CanList {
		return false, nil
	}
	col := ""
	switch scope {
	case "write":
		col = "a.can_write"
	case "delete":
		col = "a.can_delete"
	case "compute":
		col = "a.can_compute"
	default:
		return false, nil
	}

	var matched int
	err = s.store.DB.QueryRowContext(ctx, `
		SELECT COUNT(1)
		FROM mount_group_acls a
		INNER JOIN group_members gm ON gm.group_id = a.group_id
		WHERE a.node_id = ? AND a.mount_id = ? AND gm.user_id = ? AND `+col+` = 1
	`, nodeID, mountID, userID).Scan(&matched)
	if err != nil {
		return false, err
	}
	return matched > 0, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

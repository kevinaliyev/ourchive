// Package auth — Per-mount group ACL persistence and scope enforcement.
// Mounts with zero ACL rows inherit default member read/share visibility;
// mounts with ACL rows restrict non-owner access to matching group scopes.
package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

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

// CheckMountScope evaluates whether a non-owner user has the requested scope
// ("read" | "write" | "delete" | "share" | "compute") on a node's mount.
// Node owners always have full access and should bypass this check.
// If no ACL rows exist for (nodeID, mountID), default hub policy applies:
//   - "read", "share": allowed
//   - "write", "delete", "compute": denied for non-owners
func (s *Service) CheckMountScope(ctx context.Context, userID, nodeID, mountID, scope string) (bool, error) {
	var totalACLs int
	if err := s.store.DB.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM mount_group_acls WHERE node_id = ? AND mount_id = ?
	`, nodeID, mountID).Scan(&totalACLs); err != nil {
		return false, err
	}

	if totalACLs == 0 {
		return scope == "read" || scope == "share", nil
	}

	col := ""
	switch scope {
	case "read":
		col = "a.can_read"
	case "write":
		col = "a.can_write"
	case "delete":
		col = "a.can_delete"
	case "share":
		col = "a.can_share"
	case "compute":
		col = "a.can_compute"
	default:
		return false, nil
	}

	var matched int
	err := s.store.DB.QueryRowContext(ctx, `
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

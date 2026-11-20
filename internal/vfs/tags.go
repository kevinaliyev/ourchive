package vfs

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ourchive/ourchive/internal/db"
	"github.com/ourchive/ourchive/pkg/protocol"
)

var slugNonAlnum = regexp.MustCompile(`[^a-z0-9-]+`)

// NormalizeTagSlug converts arbitrary tag input into a clean lowercase slug (e.g. "2026 Clips" -> "2026-clips").
func NormalizeTagSlug(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimPrefix(s, "#")
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, " ", "-")
	s = slugNonAlnum.ReplaceAllString(s, "")
	s = strings.Trim(s, "-")
	return s
}

type TagSummary struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
	ItemCount   int    `json:"item_count"`
	CreatedAt   string `json:"created_at"`
}

type TreeItem struct {
	ID         string              `json:"id"`
	TagID      string              `json:"tag_id"`
	TagSlug    string              `json:"tag_slug"`
	NodeID     string              `json:"node_id"`
	NodeName   string              `json:"node_name"`
	NodeStatus protocol.NodeStatus `json:"node_status"`
	Traffic    protocol.TrafficStats `json:"traffic"`
	MountID    string              `json:"mount_id"`
	MountName  string              `json:"mount_name"`
	RelPath    string              `json:"rel_path"`
	IsDir      bool                `json:"is_dir"`
	CreatedAt  string              `json:"created_at"`
}

type TagStore struct {
	store *db.Store
}

func NewTagStore(store *db.Store) *TagStore {
	return &TagStore{store: store}
}

func (ts *TagStore) ListTags(ctx context.Context) ([]TagSummary, error) {
	rows, err := ts.store.DB.QueryContext(ctx, `
		SELECT t.id, t.slug, t.description, t.created_at, COUNT(ti.id)
		FROM tags t
		LEFT JOIN tagged_items ti ON ti.tag_id = t.id
		GROUP BY t.id
		ORDER BY t.slug ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tags := []TagSummary{}
	for rows.Next() {
		var s TagSummary
		if err := rows.Scan(&s.ID, &s.Slug, &s.Description, &s.CreatedAt, &s.ItemCount); err != nil {
			return nil, err
		}
		tags = append(tags, s)
	}
	return tags, rows.Err()
}

func (ts *TagStore) CreateOrGetTag(ctx context.Context, slug, description, userID string) (*TagSummary, error) {
	norm := NormalizeTagSlug(slug)
	if norm == "" {
		return nil, errors.New("valid tag name is required")
	}

	var existing TagSummary
	err := ts.store.DB.QueryRowContext(ctx,
		`SELECT id, slug, description, created_at FROM tags WHERE slug = ?`, norm,
	).Scan(&existing.ID, &existing.Slug, &existing.Description, &existing.CreatedAt)
	if err == nil {
		return &existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	tag := &TagSummary{
		ID:          uuid.NewString(),
		Slug:        norm,
		Description: strings.TrimSpace(description),
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	_, err = ts.store.DB.ExecContext(ctx,
		`INSERT INTO tags (id, slug, description, created_by, created_at) VALUES (?, ?, ?, ?, ?)`,
		tag.ID, tag.Slug, tag.Description, userID, tag.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return tag, nil
}

func (ts *TagStore) DeleteTag(ctx context.Context, tagID string) error {
	_, err := ts.store.DB.ExecContext(ctx, `DELETE FROM tags WHERE id = ?`, tagID)
	return err
}

func (ts *TagStore) AttachTag(ctx context.Context, tagSlug, nodeID, mountID, relPath string, isDir bool, userID string) (*TagSummary, error) {
	cleaned, err := CleanRelPath(relPath)
	if err != nil {
		return nil, err
	}
	tag, err := ts.CreateOrGetTag(ctx, tagSlug, "", userID)
	if err != nil {
		return nil, err
	}
	isDirInt := 0
	if isDir {
		isDirInt = 1
	}
	_, err = ts.store.DB.ExecContext(ctx, `
		INSERT OR IGNORE INTO tagged_items (id, tag_id, node_id, mount_id, rel_path, is_dir, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, uuid.NewString(), tag.ID, nodeID, mountID, cleaned, isDirInt, userID, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	return tag, nil
}

func (ts *TagStore) DetachTag(ctx context.Context, tagSlug, mountID, relPath string) error {
	cleaned, err := CleanRelPath(relPath)
	if err != nil {
		return err
	}
	norm := NormalizeTagSlug(tagSlug)
	_, err = ts.store.DB.ExecContext(ctx, `
		DELETE FROM tagged_items
		WHERE mount_id = ? AND rel_path = ? AND tag_id = (SELECT id FROM tags WHERE slug = ?)
	`, mountID, cleaned, norm)
	return err
}

// TagsByMount returns a map of rel_path -> []tag_slug for fast directory listing enrichment.
func (ts *TagStore) TagsByMount(ctx context.Context, mountID string) (map[string][]string, error) {
	rows, err := ts.store.DB.QueryContext(ctx, `
		SELECT ti.rel_path, t.slug
		FROM tagged_items ti
		JOIN tags t ON t.id = ti.tag_id
		WHERE ti.mount_id = ?
		ORDER BY t.slug ASC
	`, mountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := map[string][]string{}
	for rows.Next() {
		var rel, slug string
		if err := rows.Scan(&rel, &slug); err != nil {
			return nil, err
		}
		res[rel] = append(res[rel], slug)
	}
	return res, rows.Err()
}

// ListTreeItems returns all tagged items for a shared tree slug.
func (ts *TagStore) ListTreeItems(ctx context.Context, slug string) ([]TreeItem, error) {
	norm := NormalizeTagSlug(slug)
	rows, err := ts.store.DB.QueryContext(ctx, `
		SELECT ti.id, t.id, t.slug, ti.node_id, n.display_name, ti.mount_id, m.name, ti.rel_path, ti.is_dir, ti.created_at
		FROM tagged_items ti
		JOIN tags t ON t.id = ti.tag_id
		JOIN nodes n ON n.id = ti.node_id
		JOIN mounts m ON m.id = ti.mount_id
		WHERE t.slug = ?
		ORDER BY ti.is_dir DESC, ti.rel_path ASC
	`, norm)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []TreeItem{}
	for rows.Next() {
		var it TreeItem
		var isDirInt int
		if err := rows.Scan(
			&it.ID, &it.TagID, &it.TagSlug, &it.NodeID, &it.NodeName,
			&it.MountID, &it.MountName, &it.RelPath, &isDirInt, &it.CreatedAt,
		); err != nil {
			return nil, err
		}
		it.IsDir = isDirInt == 1
		items = append(items, it)
	}
	return items, rows.Err()
}

// CleanupDeletedPath removes any tagged_items referencing a deleted file or directory subtree.
func (ts *TagStore) CleanupDeletedPath(ctx context.Context, mountID, relPath string) {
	cleaned, err := CleanRelPath(relPath)
	if err != nil || cleaned == "" {
		return
	}
	_, _ = ts.store.DB.ExecContext(ctx,
		`DELETE FROM tagged_items WHERE mount_id = ? AND (rel_path = ? OR rel_path LIKE ?)`,
		mountID, cleaned, cleaned+"/%",
	)
}

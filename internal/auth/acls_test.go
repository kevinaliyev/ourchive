package auth

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ourchive/ourchive/internal/db"
)

func TestMountACLs(t *testing.T) {
	ctx := context.Background()
	store, err := db.OpenSQLite(ctx, filepath.Join(t.TempDir(), "acls.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	svc := NewService(store)
	admin, err := svc.SetupHostAdmin(ctx, "admin@example.com", "admin", "Admin", "supersecret123")
	if err != nil {
		t.Fatal(err)
	}
	key, err := svc.CreateInviteKey(ctx, admin, CreateInviteRequest{Role: RoleMember, MaxUses: 5}, false)
	if err != nil {
		t.Fatal(err)
	}
	alice, _, err := svc.RedeemInviteAndRegister(ctx, key.Code, "alice@example.com", "alice", "Alice", "password1234", "email_or_username")
	if err != nil {
		t.Fatal(err)
	}
	bob, _, err := svc.RedeemInviteAndRegister(ctx, key.Code, "bob@example.com", "bob", "Bob", "password1234", "email_or_username")
	if err != nil {
		t.Fatal(err)
	}

	// 1. Default policy (no ACLs): non-owner can read/share, cannot write/delete.
	if ok, _ := svc.CheckMountScope(ctx, alice.ID, "node-1", "mount-1", "read"); !ok {
		t.Fatal("expected default read allowed when no ACLs exist")
	}
	if ok, _ := svc.CheckMountScope(ctx, alice.ID, "node-1", "mount-1", "write"); ok {
		t.Fatal("expected default write denied when no ACLs exist")
	}

	// 2. Create Editors group with Alice only, attach ACL to (node-1, mount-1).
	grp, err := svc.CreateGroup(ctx, "editors", "Video editors")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.AddGroupMember(ctx, grp.ID, alice.ID, RoleMember); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpsertMountACL(ctx, MountACL{
		NodeID:   "node-1",
		MountID:  "mount-1",
		GroupID:  grp.ID,
		CanRead:  true,
		CanWrite: true,
		CanShare: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Alice (in editors) can read & write; cannot delete.
	if ok, _ := svc.CheckMountScope(ctx, alice.ID, "node-1", "mount-1", "read"); !ok {
		t.Fatal("expected alice read allowed")
	}
	if ok, _ := svc.CheckMountScope(ctx, alice.ID, "node-1", "mount-1", "write"); !ok {
		t.Fatal("expected alice write allowed")
	}
	if ok, _ := svc.CheckMountScope(ctx, alice.ID, "node-1", "mount-1", "delete"); ok {
		t.Fatal("expected alice delete denied")
	}

	// Bob (not in editors) is now blocked from reading restricted mount.
	if ok, _ := svc.CheckMountScope(ctx, bob.ID, "node-1", "mount-1", "read"); ok {
		t.Fatal("expected bob read denied on ACL-restricted mount")
	}
}

func TestShareUserRulesAndVisibility(t *testing.T) {
	ctx := context.Background()
	store, err := db.OpenSQLite(ctx, filepath.Join(t.TempDir(), "user-rules.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	svc := NewService(store)
	admin, err := svc.SetupHostAdmin(ctx, "admin@example.com", "admin", "Admin", "supersecret123")
	if err != nil {
		t.Fatal(err)
	}
	key, err := svc.CreateInviteKey(ctx, admin, CreateInviteRequest{Role: RoleMember, MaxUses: 5}, false)
	if err != nil {
		t.Fatal(err)
	}
	alice, _, err := svc.RedeemInviteAndRegister(ctx, key.Code, "alice@example.com", "alice", "Alice", "password1234", "email_or_username")
	if err != nil {
		t.Fatal(err)
	}
	bob, _, err := svc.RedeemInviteAndRegister(ctx, key.Code, "bob@example.com", "bob", "Bob", "password1234", "email_or_username")
	if err != nil {
		t.Fatal(err)
	}

	// Seed a node owned by admin with two mounts.
	if _, err := store.DB.ExecContext(ctx, `
		INSERT INTO nodes (id, owner_id, display_name, public_key, visibility, created_at)
		VALUES ('node-1', ?, 'songbird', 'pk', 'open', '2026-01-01T00:00:00Z')
	`, admin.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.ExecContext(ctx, `
		INSERT INTO mounts (id, node_id, name, local_path, kind, visibility, enabled, created_at)
		VALUES ('m-1', 'node-1', 'Clips', '/clips', 'folder', 'inherit', 1, '2026-01-01T00:00:00Z'),
		       ('m-2', 'node-1', 'Secret', '/secret', 'folder', 'private', 1, '2026-01-01T00:00:00Z')
	`); err != nil {
		t.Fatal(err)
	}

	// 1. Default open machine: Alice has full access on m-1, but no access on private m-2.
	acc, err := svc.EffectiveMountAccess(ctx, alice.ID, "node-1", "m-1")
	if err != nil || acc.Level != "all" || !acc.CanView || !acc.CanDownload {
		t.Fatalf("expected alice all on m-1, got %+v err=%v", acc, err)
	}
	if acc, _ = svc.EffectiveMountAccess(ctx, alice.ID, "node-1", "m-2"); acc.Level != "none" {
		t.Fatalf("expected alice none on private m-2, got %+v", acc)
	}

	// 2. Machine-wide rule: segregate Bob completely (none).
	if _, err := svc.UpsertShareUserRule(ctx, ShareUserRule{
		NodeID: "node-1", MountID: "", UserID: bob.ID, Access: "none",
	}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := svc.CheckMountScope(ctx, bob.ID, "node-1", "m-1", "read"); ok {
		t.Fatal("expected bob blocked on m-1 by machine-wide none rule")
	}

	// 3. Per-folder override: give Bob view-only on m-1 (can browse & preview, cannot download/share).
	if _, err := svc.UpsertShareUserRule(ctx, ShareUserRule{
		NodeID: "node-1", MountID: "m-1", UserID: bob.ID, Access: "view",
	}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := svc.CheckMountScope(ctx, bob.ID, "node-1", "m-1", "read"); !ok {
		t.Fatal("expected bob read allowed via folder view override")
	}
	if ok, _ := svc.CheckMountScope(ctx, bob.ID, "node-1", "m-1", "view"); !ok {
		t.Fatal("expected bob view allowed via folder view override")
	}
	if ok, _ := svc.CheckMountScope(ctx, bob.ID, "node-1", "m-1", "download"); ok {
		t.Fatal("expected bob download denied via folder view override")
	}
	if ok, _ := svc.CheckMountScope(ctx, bob.ID, "node-1", "m-1", "share"); ok {
		t.Fatal("expected bob share denied via folder view override")
	}

	// 4. Per-folder override: give Alice download-only on private m-2 (can list & download, cannot preview).
	if _, err := svc.UpsertShareUserRule(ctx, ShareUserRule{
		NodeID: "node-1", MountID: "m-2", UserID: alice.ID, Access: "download",
	}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := svc.CheckMountScope(ctx, alice.ID, "node-1", "m-2", "download"); !ok {
		t.Fatal("expected alice download allowed on m-2")
	}
	if ok, _ := svc.CheckMountScope(ctx, alice.ID, "node-1", "m-2", "view"); ok {
		t.Fatal("expected alice view denied on download-only m-2")
	}

	// 5. Switch machine to private: Alice (no machine rule) loses access to inherited m-1.
	if _, err := store.DB.ExecContext(ctx, `UPDATE nodes SET visibility = 'private' WHERE id = 'node-1'`); err != nil {
		t.Fatal(err)
	}
	if ok, _ := svc.CheckMountScope(ctx, alice.ID, "node-1", "m-1", "read"); ok {
		t.Fatal("expected alice denied on inherited m-1 when machine is private")
	}
}

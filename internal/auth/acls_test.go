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

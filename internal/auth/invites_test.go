package auth

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ourchive/ourchive/internal/db"
	"github.com/ourchive/ourchive/internal/settings"
)

func openTestAuth(t *testing.T) (*Service, *db.Store) {
	t.Helper()
	ctx := context.Background()
	store, err := db.OpenSQLite(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return NewService(store), store
}

func TestInviteFlowAndLineage(t *testing.T) {
	ctx := context.Background()
	svc, _ := openTestAuth(t)

	admin, err := svc.SetupHostAdmin(ctx, "admin@example.com", "admin", "Host Admin", "password123")
	if err != nil {
		t.Fatalf("setup admin: %v", err)
	}

	// Host admin creates an invite key for a group_admin
	inv, err := svc.CreateInviteKey(ctx, admin, CreateInviteRequest{
		Role:    RoleGroupAdmin,
		MaxUses: 1,
		Note:    "First friend",
	}, false)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if inv.Code == "" {
		t.Fatal("expected invite code on creation")
	}

	// Redeem using username-only under username_only mode
	friend, _, err := svc.RedeemInviteAndRegister(ctx, inv.Code, "", "alex", "Alex", "secret1234", settings.IdentityUsernameOnly)
	if err != nil {
		t.Fatalf("redeem invite: %v", err)
	}
	if friend.InvitedBy != admin.ID || friend.InviteKeyID != inv.ID {
		t.Fatalf("expected lineage invited_by=%s key=%s, got %+v", admin.ID, inv.ID, friend)
	}
	if friend.Email != "" || friend.Username != "alex" {
		t.Fatalf("expected username-only user, got email=%q username=%q", friend.Email, friend.Username)
	}

	// Authenticate by username works
	if _, err := svc.Authenticate(ctx, "alex", "secret1234"); err != nil {
		t.Fatalf("login by username failed: %v", err)
	}

	// Second use of a single-use key must fail
	if _, _, err := svc.RedeemInviteAndRegister(ctx, inv.Code, "bob@example.com", "bob", "Bob", "secret1234", settings.IdentityEmailOrUsername); err == nil {
		t.Fatal("expected exhausted invite key to fail")
	}

	// Group admin cannot invite when allowGroupAdminInvites is false
	if _, err := svc.CreateInviteKey(ctx, friend, CreateInviteRequest{MaxUses: 1}, false); err != ErrInviteForbidden {
		t.Fatalf("expected ErrInviteForbidden, got %v", err)
	}

	// Group admin CAN invite when allowGroupAdminInvites is true
	inv2, err := svc.CreateInviteKey(ctx, friend, CreateInviteRequest{MaxUses: 1, Note: "Friend of Alex"}, true)
	if err != nil {
		t.Fatalf("expected group admin invite to succeed when allowed: %v", err)
	}

	// Trusted member with quota=1
	member, _, err := svc.RedeemInviteAndRegister(ctx, inv2.Code, "sam@example.com", "sam", "Sam", "secret1234", settings.IdentityBoth)
	if err != nil {
		t.Fatalf("redeem inv2: %v", err)
	}
	if err := svc.UpdateUserInvitePerms(ctx, member.ID, true, 1); err != nil {
		t.Fatalf("grant invite perms: %v", err)
	}
	member.CanInvite = true
	member.InviteQuota = 1

	if _, err := svc.CreateInviteKey(ctx, member, CreateInviteRequest{MaxUses: 1}, false); err != nil {
		t.Fatalf("trusted member first invite failed: %v", err)
	}
	if _, err := svc.CreateInviteKey(ctx, member, CreateInviteRequest{MaxUses: 1}, false); err != ErrInviteQuotaReached {
		t.Fatalf("expected ErrInviteQuotaReached on 2nd key, got %v", err)
	}
}

func TestGroupAuditSupersededByGlobal(t *testing.T) {
	ctx := context.Background()
	svc, _ := openTestAuth(t)

	g, err := svc.CreateGroup(ctx, "Studio", "Art team")
	if err != nil {
		t.Fatalf("create group: %v", err)
	}

	// When global downloads/previews are OFF, group cannot enable them.
	if err := svc.UpdateGroupAuditSettings(ctx, g.ID, true, false, false, false); err == nil {
		t.Fatal("expected group download audit enable to be rejected when global is off")
	}
	if err := svc.UpdateGroupAuditSettings(ctx, g.ID, false, true, false, false); err == nil {
		t.Fatal("expected group preview audit enable to be rejected when global is off")
	}

	// When global is ON, group can enable them.
	if err := svc.UpdateGroupAuditSettings(ctx, g.ID, true, true, true, true); err != nil {
		t.Fatalf("expected group audit enable to succeed when global is on: %v", err)
	}
}

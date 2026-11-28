package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ourchive/ourchive/internal/audit"
	"github.com/ourchive/ourchive/internal/auth"
	"github.com/ourchive/ourchive/internal/db"
	"github.com/ourchive/ourchive/internal/events"
	"github.com/ourchive/ourchive/internal/settings"
	"github.com/ourchive/ourchive/internal/shares"
	"github.com/ourchive/ourchive/internal/tunnel"
	"github.com/ourchive/ourchive/internal/vfs"
	"github.com/ourchive/ourchive/pkg/protocol"
)

func setupTestHub(t *testing.T) (*hubApp, http.Handler) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	store, err := db.OpenSQLite(ctx, filepath.Join(tmp, "hub.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	authSvc := auth.NewService(store)
	broker := tunnel.NewBroker(store)
	tagStore := vfs.NewTagStore(store)
	settingsStore := settings.NewStore(ctx, store)
	auditSvc := audit.NewService(store, settingsStore)
	eventBus := events.NewBus()
	thumbDir := filepath.Join(tmp, "thumbs")
	shareSvc := shares.NewService(store, broker, settingsStore, "http://s.localhost:8080", thumbDir)

	app := &hubApp{
		store:         store,
		authSvc:       authSvc,
		broker:        broker,
		tagStore:      tagStore,
		settingsStore: settingsStore,
		auditSvc:      auditSvc,
		eventBus:      eventBus,
		authLimiter:   newIPRateLimiter(100, 100),
		shareSvc:      shareSvc,
		thumbDir:      thumbDir,
		hubName:       "test-hub",
		publicHubURL:  "http://localhost:8080",
		shareHost:     "s.localhost",
		shareBaseURL:  "http://s.localhost:8080",
	}

	mux := http.NewServeMux()
	app.registerSystemRoutes(mux)
	app.registerAuthRoutes(mux)
	app.registerNodeRoutes(mux)
	app.registerVFSRoutes(mux)
	app.registerShareRoutes(mux)
	app.registerComputeRoutes(mux)
	app.registerClientRoutes(mux)
	return app, mux
}

func makeSessionCookie(t *testing.T, authSvc *auth.Service, userID string) *http.Cookie {
	t.Helper()
	sid, _, err := authSvc.CreateSession(context.Background(), userID, time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return &http.Cookie{Name: auth.SessionCookieName, Value: sid}
}

func enrollTestNode(t *testing.T, broker *tunnel.Broker, ownerID, name string) string {
	t.Helper()
	tok, _, err := broker.CreateEnrollToken(context.Background(), ownerID, name)
	if err != nil {
		t.Fatalf("CreateEnrollToken: %v", err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	res, err := broker.EnrollNode(context.Background(), protocol.EnrollRequest{
		Token:       tok,
		DisplayName: name,
		PublicKey:   base64.StdEncoding.EncodeToString(pub),
		OS:          "linux",
		Arch:        "amd64",
	})
	if err != nil {
		t.Fatalf("EnrollNode: %v", err)
	}
	return res.NodeID
}

func doReq(t *testing.T, h http.Handler, method, target string, cookie *http.Cookie, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, target, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSecurityPermissionsAndPrivilegeEscalation(t *testing.T) {
	ctx := context.Background()
	app, handler := setupTestHub(t)

	// Create Host Admin, Group Admin (groupA), Alice (member, node owner), Bob (member, node owner).
	admin, err := app.authSvc.SetupHostAdmin(ctx, "admin@example.com", "admin", "Admin", "password1234")
	if err != nil {
		t.Fatal(err)
	}
	groupAdmin, err := app.authSvc.CreateUser(ctx, "gadmin@example.com", "GAdmin", "password1234", auth.RoleGroupAdmin)
	if err != nil {
		t.Fatal(err)
	}
	alice, err := app.authSvc.CreateUser(ctx, "alice@example.com", "Alice", "password1234", auth.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := app.authSvc.CreateUser(ctx, "bob@example.com", "Bob", "password1234", auth.RoleMember)
	if err != nil {
		t.Fatal(err)
	}

	adminCookie := makeSessionCookie(t, app.authSvc, admin.ID)
	gadminCookie := makeSessionCookie(t, app.authSvc, groupAdmin.ID)
	aliceCookie := makeSessionCookie(t, app.authSvc, alice.ID)
	bobCookie := makeSessionCookie(t, app.authSvc, bob.ID)

	// Create two groups: groupA (managed by groupAdmin) and groupB (private, NOT managed by groupAdmin).
	groupA, err := app.authSvc.CreateGroup(ctx, "group-a", "Group A")
	if err != nil {
		t.Fatal(err)
	}
	groupB, err := app.authSvc.CreateGroup(ctx, "group-b", "Group B")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.authSvc.AddGroupMember(ctx, groupA.ID, groupAdmin.ID, "admin"); err != nil {
		t.Fatal(err)
	}

	// 1. Group Admin privilege escalation checks:
	// groupAdmin must NOT be able to add themselves to groupB.
	if rec := doReq(t, handler, http.MethodPost, "/api/groups/"+groupB.ID+"/members", gadminCookie, map[string]any{
		"user_id": groupAdmin.ID,
		"role":    "admin",
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when group_admin adds member to unmanaged groupB, got %d: %s", rec.Code, rec.Body.String())
	}
	// groupAdmin must NOT be able to mint an invite key bound to unmanaged groupB.
	if rec := doReq(t, handler, http.MethodPost, "/api/invites", gadminCookie, map[string]any{
		"role":     "member",
		"group_id": groupB.ID,
		"max_uses": 1,
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when group_admin mints invite for unmanaged groupB, got %d: %s", rec.Code, rec.Body.String())
	}
	// groupAdmin CAN add alice to groupA (which they administer).
	if rec := doReq(t, handler, http.MethodPost, "/api/groups/"+groupA.ID+"/members", gadminCookie, map[string]any{
		"user_id": alice.ID,
		"role":    "member",
	}); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when group_admin adds member to own groupA, got %d: %s", rec.Code, rec.Body.String())
	}

	// Enroll Alice's node and Bob's node, and add a mount to Bob's node restricted to groupB.
	aliceNode := enrollTestNode(t, app.broker, alice.ID, "alice-pc")
	bobNode := enrollTestNode(t, app.broker, bob.ID, "bob-pc")
	mountRec := doReq(t, handler, http.MethodPost, "/api/nodes/"+bobNode+"/mounts", bobCookie, map[string]any{
		"name":       "Private",
		"local_path": t.TempDir(),
		"kind":       "folder",
	})
	if mountRec.Code != http.StatusCreated {
		t.Fatalf("create bob mount failed: %d %s", mountRec.Code, mountRec.Body.String())
	}
	var mountRes struct {
		Mount protocol.FolderMount `json:"mount"`
	}
	_ = json.Unmarshal(mountRec.Body.Bytes(), &mountRes)
	bobMountID := mountRes.Mount.ID

	aclRec := doReq(t, handler, http.MethodPut, "/api/acls", bobCookie, auth.MountACL{
		NodeID:   bobNode,
		MountID:  bobMountID,
		GroupID:  groupB.ID,
		CanRead:  true,
		CanShare: true,
	})
	if aclRec.Code != http.StatusOK {
		t.Fatalf("create bob ACL failed: %d %s", aclRec.Code, aclRec.Body.String())
	}
	var aclRes struct {
		ACL auth.MountACL `json:"acl"`
	}
	_ = json.Unmarshal(aclRec.Body.Bytes(), &aclRes)
	bobACLID := aclRes.ACL.ID

	// 2. IDOR check: Alice cannot delete Bob's ACL by passing her own node_id in query string.
	if rec := doReq(t, handler, http.MethodDelete, "/api/acls/"+bobACLID+"?node_id="+aliceNode, aliceCookie, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on cross-node ACL delete IDOR, got %d: %s", rec.Code, rec.Body.String())
	}

	// 3. ACL enforcement on file read, thumb, share creation, and tag attach:
	// Alice is NOT in groupB, so all of these must return 403 Forbidden.
	if rec := doReq(t, handler, http.MethodGet, "/api/vfs/nodes/"+bobNode+"/mounts/"+bobMountID+"/file?path=secret.txt", aliceCookie, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on restricted /file read, got %d", rec.Code)
	}
	if rec := doReq(t, handler, http.MethodGet, "/api/vfs/nodes/"+bobNode+"/mounts/"+bobMountID+"/thumb?path=secret.png", aliceCookie, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on restricted /thumb read, got %d", rec.Code)
	}
	if rec := doReq(t, handler, http.MethodPost, "/api/shares", aliceCookie, map[string]any{
		"node_id":  bobNode,
		"mount_id": bobMountID,
		"rel_path": "secret.txt",
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on restricted POST /api/shares, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, handler, http.MethodPost, "/api/tags/attach", aliceCookie, map[string]any{
		"tag_slug": "leaked",
		"node_id":  bobNode,
		"mount_id": bobMountID,
		"rel_path": "secret.txt",
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on restricted POST /api/tags/attach, got %d: %s", rec.Code, rec.Body.String())
	}

	// 4. Strict node ownership invariant: Host Admin is NOT owner of Bob's machine.
	nodesRec := doReq(t, handler, http.MethodGet, "/api/nodes", adminCookie, nil)
	var nodesRes struct {
		Nodes []tunnel.NodeView `json:"nodes"`
	}
	_ = json.Unmarshal(nodesRec.Body.Bytes(), &nodesRes)
	for _, nv := range nodesRes.Nodes {
		if nv.ID == bobNode && nv.IsOwner {
			t.Fatal("host_admin must have is_owner=false on another user's node")
		}
	}
	// Host Admin cannot grant write/delete ACLs on Bob's node.
	if rec := doReq(t, handler, http.MethodPut, "/api/acls", adminCookie, auth.MountACL{
		NodeID:   bobNode,
		MountID:  bobMountID,
		GroupID:  groupA.ID,
		CanRead:  true,
		CanWrite: true,
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when host_admin tries to grant write ACL on bob's node, got %d", rec.Code)
	}
	// Host Admin cannot mutate Bob's upload cap, but CAN pause Bob's node.
	if rec := doReq(t, handler, http.MethodPatch, "/api/nodes/"+bobNode+"/config", adminCookie, map[string]any{
		"upload_cap_mbps": 10,
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when host_admin tries to change bob's upload cap, got %d", rec.Code)
	}
	if rec := doReq(t, handler, http.MethodPatch, "/api/nodes/"+bobNode+"/config", adminCookie, map[string]any{
		"paused": true,
	}); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when host_admin pauses bob's node, got %d: %s", rec.Code, rec.Body.String())
	}
}

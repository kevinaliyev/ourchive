package tunnel

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/ourchive/ourchive/internal/auth"
	"github.com/ourchive/ourchive/internal/db"
	"github.com/ourchive/ourchive/pkg/protocol"
)

func TestPhase1AuthEnrollTunnelAndConfigPush(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "ourchive-test.db")
	store, err := db.OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer store.Close()

	// 1. Auth: first-run host_admin setup, Argon2id verify, sessions, groups
	authSvc := auth.NewService(store)
	if count, err := authSvc.UserCount(ctx); err != nil || count != 0 {
		t.Fatalf("expected 0 users initially, got %d (%v)", count, err)
	}

	admin, err := authSvc.SetupHostAdmin(ctx, "ru@ourchive.local", "ru", "Ru", "supersecret99")
	if err != nil {
		t.Fatalf("SetupHostAdmin: %v", err)
	}
	if admin.Role != auth.RoleHostAdmin {
		t.Fatalf("expected host_admin, got %s", admin.Role)
	}
	if _, err := authSvc.SetupHostAdmin(ctx, "other@ourchive.local", "other", "Other", "supersecret99"); err != auth.ErrAlreadyInitialized {
		t.Fatalf("expected ErrAlreadyInitialized on second setup, got %v", err)
	}
	if _, err := authSvc.Authenticate(ctx, "ru@ourchive.local", "wrongpass"); err != auth.ErrInvalidCredentials {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
	authed, err := authSvc.Authenticate(ctx, "ru@ourchive.local", "supersecret99")
	if err != nil || authed.ID != admin.ID {
		t.Fatalf("Authenticate failed: %v", err)
	}
	sid, _, err := authSvc.CreateSession(ctx, admin.ID, time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sessUser, err := authSvc.GetSessionUser(ctx, sid); err != nil || sessUser.ID != admin.ID {
		t.Fatalf("GetSessionUser failed: %v", err)
	}

	// 2. Enrollment token & Ed25519 Agent enrollment
	broker := NewBroker(store)
	tok, _, err := broker.CreateEnrollToken(ctx, admin.ID, "songbird")
	if err != nil {
		t.Fatalf("CreateEnrollToken: %v", err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	enrolled, err := broker.EnrollNode(ctx, protocol.EnrollRequest{
		Token:       tok,
		DisplayName: "songbird",
		PublicKey:   base64.StdEncoding.EncodeToString(pub),
		OS:          "windows",
		Arch:        "amd64",
		Compute: protocol.ComputeCapabilities{
			Enabled:    true,
			CPUCores:   16,
			HWEncoders: []string{"h264_nvenc"},
		},
	})
	if err != nil {
		t.Fatalf("EnrollNode: %v", err)
	}

	// Token must be single-use.
	if _, err := broker.EnrollNode(ctx, protocol.EnrollRequest{
		Token:     tok,
		PublicKey: base64.StdEncoding.EncodeToString(pub),
	}); err == nil {
		t.Fatalf("expected second use of enrollment token to fail")
	}

	// 3. Live WSS + yamux tunnel & Hub -> Agent config push
	srv := httptest.NewServer(http.HandlerFunc(broker.HandleAgentTunnel))
	defer srv.Close()

	ts := time.Now().Unix()
	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) +
		fmt.Sprintf("?node_id=%s&ts=%d&sig=%s", enrolled.NodeID, ts, SignHandshake(enrolled.NodeID, ts, priv))

	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(dialCtx, wsURL, nil)
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}

	connCtx, cancelConn := context.WithCancel(ctx)
	defer cancelConn()
	netConn := websocket.NetConn(connCtx, ws, websocket.MessageBinary)
	ycfg := yamux.DefaultConfig()
	ycfg.LogOutput = io.Discard
	clientSess, err := yamux.Client(netConn, ycfg)
	if err != nil {
		t.Fatalf("yamux.Client: %v", err)
	}
	defer clientSess.Close()

	ctrl, err := clientSess.Open()
	if err != nil {
		t.Fatalf("open control stream: %v", err)
	}
	defer ctrl.Close()

	// Send initial hello manifest.
	enc := json.NewEncoder(ctrl)
	if err := enc.Encode(protocol.ControlEnvelope{
		Type: protocol.ControlHello,
		Manifest: &protocol.NodeManifest{
			NodeID:      enrolled.NodeID,
			DisplayName: "songbird",
			OS:          "windows",
			Arch:        "amd64",
			Status:      protocol.NodeStatusOnline,
			Compute:     protocol.ComputeCapabilities{Enabled: true, CPUCores: 16, HWEncoders: []string{"h264_nvenc"}},
			UpdatedAt:   time.Now().UTC(),
		},
	}); err != nil {
		t.Fatalf("send hello: %v", err)
	}

	// Read initial config_push sent by Hub on connect.
	scanner := bufio.NewScanner(ctrl)
	if !scanner.Scan() {
		t.Fatalf("expected initial config_push from hub")
	}
	var initialEnv protocol.ControlEnvelope
	if err := json.Unmarshal(scanner.Bytes(), &initialEnv); err != nil {
		t.Fatalf("decode initial config_push: %v", err)
	}
	if initialEnv.Type != protocol.ControlConfigPush || initialEnv.Config == nil {
		t.Fatalf("unexpected envelope: %+v", initialEnv)
	}

	// Update node config & add a folder mount in DB, then push live to Agent.
	_, err = store.DB.ExecContext(ctx,
		`UPDATE nodes SET upload_cap_mbps = 25, allow_group_compute = 1 WHERE id = ?`, enrolled.NodeID)
	if err != nil {
		t.Fatalf("update node: %v", err)
	}
	_, err = store.DB.ExecContext(ctx,
		`INSERT INTO mounts (id, node_id, name, local_path, enabled, created_at) VALUES ('m1', ?, 'Clips', 'C:\Videos', 1, ?)`,
		enrolled.NodeID, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert mount: %v", err)
	}
	broker.PushConfig(ctx, enrolled.NodeID)

	if !scanner.Scan() {
		t.Fatalf("expected live config_push after PushConfig")
	}
	var updatedEnv protocol.ControlEnvelope
	if err := json.Unmarshal(scanner.Bytes(), &updatedEnv); err != nil {
		t.Fatalf("decode updated config_push: %v", err)
	}
	if updatedEnv.Config.UploadCapMbps != 25 || !updatedEnv.Config.AllowGroupCompute || len(updatedEnv.Config.Mounts) != 1 {
		t.Fatalf("unexpected pushed config: %+v", updatedEnv.Config)
	}

	// Verify ListNodesForUser reflects online status + mount.
	nodes, err := broker.ListNodesForUser(ctx, admin)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("ListNodesForUser: len=%d err=%v", len(nodes), err)
	}
	if nodes[0].Status != protocol.NodeStatusOnline || nodes[0].UploadCapMbps != 25 || len(nodes[0].Mounts) != 1 {
		t.Fatalf("unexpected node view: %+v", nodes[0])
	}
}

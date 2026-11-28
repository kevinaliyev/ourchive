package agentcore

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ourchive/ourchive/internal/agentstatus"
	"github.com/ourchive/ourchive/internal/auth"
	"github.com/ourchive/ourchive/internal/db"
	"github.com/ourchive/ourchive/internal/tunnel"
	"github.com/ourchive/ourchive/pkg/protocol"
)

// TestEnrollAndRunAgainstRealBroker drives the real enroll call and tunnel (WSS + yamux +
// control stream) through the Runner, then takes the hub away and brings it back.
func TestEnrollAndRunAgainstRealBroker(t *testing.T) {
	ctx := context.Background()
	store, err := db.OpenSQLite(ctx, filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer store.Close()

	authSvc := auth.NewService(store)
	admin, err := authSvc.SetupHostAdmin(ctx, "ru@ourchive.local", "ru", "Ru", "supersecret99")
	if err != nil {
		t.Fatalf("SetupHostAdmin: %v", err)
	}
	broker := tunnel.NewBroker(store)
	token, _, err := broker.CreateEnrollToken(ctx, admin.ID, "songbird")
	if err != nil {
		t.Fatalf("CreateEnrollToken: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/agent/enroll", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.EnrollRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		res, err := broker.EnrollNode(r.Context(), req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(res)
	})
	mux.HandleFunc("GET /api/agent/tunnel", broker.HandleAgentTunnel)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfgPath := filepath.Join(t.TempDir(), "hubs", "test.json")
	res, err := Enroll(ctx, EnrollOptions{HubURL: srv.URL + "/", Token: token, Name: "songbird", ConfigPath: cfgPath})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if res.HubURL != srv.URL || res.NodeID == "" || res.ConfigPath != cfgPath {
		t.Fatalf("unexpected enroll result %+v", res)
	}
	if _, err := Enroll(ctx, EnrollOptions{HubURL: srv.URL, Token: token, ConfigPath: cfgPath + ".2"}); err == nil {
		t.Fatalf("expected reused token to be rejected")
	}

	caps := protocol.ComputeCapabilities{}
	r := New(Options{ConfigPaths: []string{cfgPath}, MaxRetries: 2, InitialBackoff: 5 * time.Millisecond, MaxBackoff: 10 * time.Millisecond, Compute: &caps})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { _ = r.Run(runCtx); close(done) }()

	connected := waitForState(t, r, srv.URL, agentstatus.StateConnected)
	if connected.NodeID != res.NodeID {
		t.Fatalf("status node id %q, want %q", connected.NodeID, res.NodeID)
	}

	// Wait until the broker has registered the live tunnel, then kick the node from the hub side:
	// the runner must notice and reconnect on its own with a fresh connection.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := broker.HTTPClientForNode(res.NodeID); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("broker never registered the agent tunnel")
		}
		time.Sleep(5 * time.Millisecond)
	}
	firstSince := *connected.ConnectedSince
	broker.DisconnectNode(res.NodeID)
	deadline = time.Now().Add(5 * time.Second)
	for {
		st := r.Status()[0]
		if st.State == agentstatus.StateConnected && st.ConnectedSince != nil && st.ConnectedSince.After(firstSince) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("runner did not reconnect after hub-side disconnect; status %+v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

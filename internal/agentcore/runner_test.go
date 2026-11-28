package agentcore

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ourchive/ourchive/internal/agentstatus"
	"github.com/ourchive/ourchive/pkg/protocol"
)

func writeTestConfig(t *testing.T, dir, hubURL string) string {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, HubSlug(hubURL)+".json")
	if err := SaveConfig(path, &AgentConfig{
		HubURL:      hubURL + "/",
		NodeID:      "node-" + HubSlug(hubURL),
		DisplayName: "test machine",
		PublicKey:   base64.StdEncoding.EncodeToString(pub),
		PrivateKey:  base64.StdEncoding.EncodeToString(priv),
	}); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitForState(t *testing.T, r *Runner, hubURL string, want agentstatus.State) agentstatus.HubStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, h := range r.Status() {
			if h.HubURL == hubURL && h.State == want {
				return h
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("hub %s never reached state %q; last status: %+v", hubURL, want, r.Status())
	return agentstatus.HubStatus{}
}

func newTestRunner(t *testing.T, paths []string, maxRetries int) *Runner {
	t.Helper()
	caps := protocol.ComputeCapabilities{}
	return New(Options{
		ConfigPaths:    paths,
		MaxRetries:     maxRetries,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
		Compute:        &caps,
	})
}

func TestRunnerParksFailedHubUntilRetry(t *testing.T) {
	dir := t.TempDir()
	hub := "https://hub-a.test"
	r := newTestRunner(t, []string{writeTestConfig(t, dir, hub)}, 3)

	var healthy atomic.Bool
	var attempts atomic.Int32
	r.connect = func(ctx context.Context, w *worker, onConnected func()) error {
		attempts.Add(1)
		if !healthy.Load() {
			return errors.New("dial hub: connection refused")
		}
		onConnected()
		<-ctx.Done()
		return ctx.Err()
	}

	if got := r.Hubs(); len(got) != 1 || got[0] != hub {
		t.Fatalf("expected trailing slash to be normalized away, got hubs %v", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()

	failed := waitForState(t, r, hub, agentstatus.StateFailed)
	if failed.LastError == "" || failed.Failures != 3 {
		t.Fatalf("expected failure details on parked hub, got %+v", failed)
	}
	if n := attempts.Load(); n != 3 {
		t.Fatalf("expected exactly 3 attempts before parking, got %d", n)
	}
	if agentstatus.Summarize(r.Status()) != agentstatus.SummaryFailed {
		t.Fatalf("expected summary failed")
	}

	// Parked hubs must not keep dialing.
	time.Sleep(20 * time.Millisecond)
	if n := attempts.Load(); n != 3 {
		t.Fatalf("parked hub kept retrying (%d attempts)", n)
	}

	healthy.Store(true)
	if !r.Retry(hub + "/") {
		t.Fatalf("Retry should find the hub regardless of trailing slash")
	}
	if s := r.Status()[0].State; s == agentstatus.StateFailed {
		t.Fatalf("Retry must clear the failed state synchronously, got %q", s)
	}
	ok := waitForState(t, r, hub, agentstatus.StateConnected)
	if ok.ConnectedSince == nil || ok.LastError != "" {
		t.Fatalf("expected clean connected status, got %+v", ok)
	}
	if agentstatus.Summarize(r.Status()) != agentstatus.SummaryOK {
		t.Fatalf("expected summary ok")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunnerAddAndRemoveHubWhileRunning(t *testing.T) {
	dir := t.TempDir()
	r := newTestRunner(t, []string{filepath.Join(dir, "missing.json")}, 0)
	if len(r.Hubs()) != 0 || len(r.LoadErrors()) != 1 {
		t.Fatalf("expected the missing config to be skipped and reported")
	}
	r.connect = func(ctx context.Context, w *worker, onConnected func()) error {
		onConnected()
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx) }()

	if agentstatus.Summarize(r.Status()) != agentstatus.SummaryNone {
		t.Fatalf("expected summary none with no hubs")
	}

	hub := "https://hub-b.test"
	if err := r.AddHub(writeTestConfig(t, dir, hub)); err != nil {
		t.Fatalf("AddHub: %v", err)
	}
	waitForState(t, r, hub, agentstatus.StateConnected)

	// Re-adding the same hub replaces the running connection instead of duplicating it.
	if err := r.AddHub(writeTestConfig(t, dir, hub)); err != nil {
		t.Fatalf("AddHub again: %v", err)
	}
	waitForState(t, r, hub, agentstatus.StateConnected)
	if n := len(r.Hubs()); n != 1 {
		t.Fatalf("expected 1 hub after re-add, got %d", n)
	}

	if !r.RemoveHub(hub) {
		t.Fatalf("RemoveHub returned false")
	}
	if len(r.Status()) != 0 {
		t.Fatalf("expected removed hub to leave the board, got %+v", r.Status())
	}
}

func TestBoardSubscribeDeliversLatestSnapshot(t *testing.T) {
	b := agentstatus.NewBoard()
	ch, unsubscribe := b.Subscribe()
	if snap := <-ch; len(snap) != 0 {
		t.Fatalf("expected an empty initial snapshot, got %+v", snap)
	}
	// Several updates without a reader must collapse to the newest one.
	b.Update("https://x.test", func(h *agentstatus.HubStatus) { h.State = agentstatus.StateConnecting })
	b.Update("https://x.test", func(h *agentstatus.HubStatus) { h.State = agentstatus.StateRetrying })
	b.Update("https://x.test", func(h *agentstatus.HubStatus) { h.State = agentstatus.StateConnected })
	snap := <-ch
	if len(snap) != 1 || snap[0].State != agentstatus.StateConnected {
		t.Fatalf("expected the latest snapshot, got %+v", snap)
	}
	unsubscribe()
	unsubscribe() // idempotent
	if _, open := <-ch; open {
		t.Fatalf("expected channel to be closed after unsubscribe")
	}
}

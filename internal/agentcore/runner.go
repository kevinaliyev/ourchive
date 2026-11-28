package agentcore

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/ourchive/ourchive/internal/agentstatus"
	"github.com/ourchive/ourchive/internal/compute"
	"github.com/ourchive/ourchive/pkg/protocol"
)

// Options configures a Runner.
type Options struct {
	// ConfigPaths lists hub config files to load. Empty means DiscoverHubConfigs().
	ConfigPaths []string
	// ForceReadOnly forces read-only mode on every hub (blocks all remote writes/deletes).
	ForceReadOnly bool
	// MaxRetries is the number of consecutive failed attempts before a hub stops and waits for
	// Retry. Zero or less retries forever.
	MaxRetries int
	// InitialBackoff and MaxBackoff bound the reconnect delay. Defaults: 1s and 15s.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// Compute overrides hardware probing (tests). Nil probes lazily on the first connection.
	Compute *protocol.ComputeCapabilities
}

// connectFunc dials one hub and serves it until the connection ends.
type connectFunc func(ctx context.Context, w *worker, onConnected func()) error

// Runner keeps every enrolled hub connected, each in its own goroutine. Hubs that exhaust their
// retries park in agentstatus.StateFailed until Retry is called; the Runner itself never exits
// because of connection failures.
type Runner struct {
	opts  Options
	board *agentstatus.Board

	computeOnce sync.Once
	computeCaps protocol.ComputeCapabilities

	mu       sync.Mutex
	workers  map[string]*worker
	runCtx   context.Context
	wg       sync.WaitGroup
	loadErrs []error

	connect connectFunc
}

type worker struct {
	path    string
	hubURL  string
	cfg     *AgentConfig
	cfgMu   sync.RWMutex
	privKey ed25519.PrivateKey
	wake    chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}
}

// New loads the configured hubs. Invalid config files are skipped and reported by LoadErrors;
// a Runner with zero hubs is valid (hubs can be added later with AddHub).
func New(opts Options) *Runner {
	if opts.InitialBackoff <= 0 {
		opts.InitialBackoff = time.Second
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 15 * time.Second
	}
	r := &Runner{
		opts:    opts,
		board:   agentstatus.NewBoard(),
		workers: map[string]*worker{},
	}
	r.connect = r.connectAndServe
	if opts.Compute != nil {
		r.computeOnce.Do(func() { r.computeCaps = *opts.Compute })
	}

	paths := opts.ConfigPaths
	if len(paths) == 0 {
		paths = DiscoverHubConfigs()
	}
	for _, p := range paths {
		w, err := r.loadWorker(p)
		if err != nil {
			log.Printf("skipping config %s: %v", p, err)
			r.loadErrs = append(r.loadErrs, fmt.Errorf("%s: %w", p, err))
			continue
		}
		r.workers[w.hubURL] = w
		r.publishIdle(w)
	}
	return r
}

// LoadErrors reports config files New skipped.
func (r *Runner) LoadErrors() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]error(nil), r.loadErrs...)
}

// Hubs returns the URLs of all loaded hubs, sorted.
func (r *Runner) Hubs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.workers))
	for u := range r.workers {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// Status returns the current state of every hub.
func (r *Runner) Status() []agentstatus.HubStatus { return r.board.Snapshot() }

// Subscribe streams status snapshots; see agentstatus.Board.Subscribe.
func (r *Runner) Subscribe() (<-chan []agentstatus.HubStatus, func()) { return r.board.Subscribe() }

// Run connects every loaded hub and blocks until ctx is cancelled and all connections stop.
func (r *Runner) Run(ctx context.Context) error {
	r.mu.Lock()
	if r.runCtx != nil {
		r.mu.Unlock()
		return errors.New("agentcore: Runner.Run called twice")
	}
	r.runCtx = ctx
	for _, w := range r.workers {
		r.startLocked(w)
	}
	r.mu.Unlock()

	<-ctx.Done()
	r.wg.Wait()
	return nil
}

// AddHub loads (or reloads) a hub config and, if the Runner is running, connects it right away.
// An existing connection to the same hub is stopped and replaced.
func (r *Runner) AddHub(configPath string) error {
	w, err := r.loadWorker(configPath)
	if err != nil {
		return err
	}
	r.mu.Lock()
	old := r.workers[w.hubURL]
	r.mu.Unlock()
	if old != nil {
		r.stopWorker(old)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.workers[w.hubURL] = w
	r.publishIdle(w)
	if r.runCtx != nil {
		r.startLocked(w)
	}
	return nil
}

// RemoveHub disconnects a hub and forgets it for this process. Its config file is left on disk.
func (r *Runner) RemoveHub(hubURL string) bool {
	hubURL = NormalizeHubURL(hubURL)
	r.mu.Lock()
	w := r.workers[hubURL]
	delete(r.workers, hubURL)
	r.mu.Unlock()
	if w == nil {
		return false
	}
	r.stopWorker(w)
	r.board.Remove(hubURL)
	return true
}

// Retry wakes a hub that is waiting out a backoff or parked after exhausting its retries.
func (r *Runner) Retry(hubURL string) bool {
	hubURL = NormalizeHubURL(hubURL)
	r.mu.Lock()
	w := r.workers[hubURL]
	r.mu.Unlock()
	if w == nil {
		return false
	}
	r.wakeWorker(w)
	return true
}

// RetryAll wakes every hub.
func (r *Runner) RetryAll() {
	r.mu.Lock()
	ws := make([]*worker, 0, len(r.workers))
	for _, w := range r.workers {
		ws = append(ws, w)
	}
	r.mu.Unlock()
	for _, w := range ws {
		r.wakeWorker(w)
	}
}

func (r *Runner) wakeWorker(w *worker) {
	// Mark a parked hub as connecting before waking it so observers never see a stale
	// "failed" snapshot after asking for a retry.
	r.board.Update(w.hubURL, func(h *agentstatus.HubStatus) {
		if h.State == agentstatus.StateFailed {
			h.State = agentstatus.StateConnecting
			h.NextRetryAt = nil
		}
	})
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (r *Runner) loadWorker(path string) (*worker, error) {
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	privBytes, err := base64.StdEncoding.DecodeString(cfg.PrivateKey)
	if err != nil || len(privBytes) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid private_key")
	}
	cfg.HubURL = NormalizeHubURL(cfg.HubURL)
	if cfg.HubURL == "" {
		return nil, errors.New("missing hub_url")
	}
	if r.opts.ForceReadOnly {
		cfg.ReadOnly = true
	}
	return &worker{
		path:    path,
		hubURL:  cfg.HubURL,
		cfg:     cfg,
		privKey: ed25519.PrivateKey(privBytes),
		wake:    make(chan struct{}, 1),
	}, nil
}

func (r *Runner) publishIdle(w *worker) {
	r.board.Update(w.hubURL, func(h *agentstatus.HubStatus) {
		*h = agentstatus.HubStatus{
			NodeID:      w.cfg.NodeID,
			DisplayName: w.cfg.DisplayName,
			State:       agentstatus.StateIdle,
			MaxRetries:  r.opts.MaxRetries,
		}
	})
}

func (r *Runner) startLocked(w *worker) {
	wctx, cancel := context.WithCancel(r.runCtx)
	w.cancel = cancel
	w.done = make(chan struct{})
	r.wg.Add(1)
	go r.runWorker(wctx, w)
}

func (r *Runner) stopWorker(w *worker) {
	r.mu.Lock()
	cancel, done := w.cancel, w.done
	r.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

func (r *Runner) runWorker(ctx context.Context, w *worker) {
	defer r.wg.Done()
	defer close(w.done)

	hooks := loopHooks{
		onAttempt: func(failures int) {
			r.board.Update(w.hubURL, func(h *agentstatus.HubStatus) {
				h.State = agentstatus.StateConnecting
				h.Failures = failures
				h.NextRetryAt = nil
			})
		},
		onConnected: func() {
			now := time.Now().UTC()
			r.board.Update(w.hubURL, func(h *agentstatus.HubStatus) {
				h.State = agentstatus.StateConnected
				h.Failures = 0
				h.LastError = ""
				h.NextRetryAt = nil
				h.ConnectedSince = &now
			})
		},
		onRetry: func(failures int, err error, wait time.Duration) {
			next := time.Now().Add(wait).UTC()
			r.board.Update(w.hubURL, func(h *agentstatus.HubStatus) {
				h.State = agentstatus.StateRetrying
				h.Failures = failures
				h.LastError = errString(err)
				h.NextRetryAt = &next
				h.ConnectedSince = nil
			})
		},
	}

	for {
		err := runHubLoopWithConnect(ctx, w.hubURL, r.opts.MaxRetries, r.opts.InitialBackoff, r.opts.MaxBackoff,
			func(cctx context.Context, onConnected func()) error { return r.connect(cctx, w, onConnected) },
			hooks, w.wake)
		if ctx.Err() != nil {
			return
		}
		r.board.Update(w.hubURL, func(h *agentstatus.HubStatus) {
			h.State = agentstatus.StateFailed
			h.Failures = r.opts.MaxRetries
			h.LastError = errString(err)
			h.NextRetryAt = nil
			h.ConnectedSince = nil
		})
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		}
	}
}

func (r *Runner) computeCapabilities() protocol.ComputeCapabilities {
	r.computeOnce.Do(func() { r.computeCaps = compute.ProbeCompute() })
	return r.computeCaps
}

func (r *Runner) connectAndServe(ctx context.Context, w *worker, onConnected func()) error {
	return connectAndServe(ctx, w.path, w.cfg, &w.cfgMu, w.privKey, r.computeCapabilities(), onConnected)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

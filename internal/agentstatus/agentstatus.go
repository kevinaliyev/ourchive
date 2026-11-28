// Package agentstatus describes the live connection state of each enrolled hub so that any
// front end (the CLI prompt, the desktop tray, the client UI) can observe the agent without
// reading its logs or owning its terminal.
package agentstatus

import (
	"sort"
	"sync"
	"time"
)

// State is the connection state of one hub.
type State string

const (
	// StateIdle means the hub is enrolled but its connection loop has not started.
	StateIdle State = "idle"
	// StateConnecting means a dial attempt is in progress.
	StateConnecting State = "connecting"
	// StateConnected means the tunnel is up and serving.
	StateConnected State = "connected"
	// StateRetrying means the last attempt failed and another is scheduled.
	StateRetrying State = "retrying"
	// StateFailed means retries were exhausted; the hub waits for a manual retry.
	StateFailed State = "failed"
)

// HubStatus is a point-in-time view of one hub connection.
type HubStatus struct {
	HubURL         string     `json:"hub_url"`
	NodeID         string     `json:"node_id,omitempty"`
	DisplayName    string     `json:"display_name,omitempty"`
	State          State      `json:"state"`
	Failures       int        `json:"failures"`
	MaxRetries     int        `json:"max_retries"`
	LastError      string     `json:"last_error,omitempty"`
	NextRetryAt    *time.Time `json:"next_retry_at,omitempty"`
	ConnectedSince *time.Time `json:"connected_since,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// Summary is the aggregate state used for a single tray icon or exit decision.
type Summary string

const (
	// SummaryNone means no hubs are enrolled.
	SummaryNone Summary = "none"
	// SummaryOK means every hub is connected.
	SummaryOK Summary = "ok"
	// SummaryDegraded means at least one hub is connecting, retrying or failed, but not all failed.
	SummaryDegraded Summary = "degraded"
	// SummaryFailed means every hub has exhausted its retries.
	SummaryFailed Summary = "failed"
)

// Summarize reduces a snapshot to one aggregate state.
func Summarize(hubs []HubStatus) Summary {
	if len(hubs) == 0 {
		return SummaryNone
	}
	connected, failed := 0, 0
	for _, h := range hubs {
		switch h.State {
		case StateConnected:
			connected++
		case StateFailed:
			failed++
		}
	}
	switch {
	case failed == len(hubs):
		return SummaryFailed
	case connected == len(hubs):
		return SummaryOK
	default:
		return SummaryDegraded
	}
}

// Board holds the current status of every hub and fans changes out to subscribers.
// Subscribers always receive the latest snapshot; intermediate snapshots may be dropped.
type Board struct {
	mu     sync.Mutex
	hubs   map[string]*HubStatus
	subs   map[int]chan []HubStatus
	nextID int
	now    func() time.Time
}

// NewBoard returns an empty board.
func NewBoard() *Board {
	return &Board{
		hubs: map[string]*HubStatus{},
		subs: map[int]chan []HubStatus{},
		now:  time.Now,
	}
}

// Update applies fn to the status of hubURL (creating it if needed) and notifies subscribers.
func (b *Board) Update(hubURL string, fn func(*HubStatus)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h, ok := b.hubs[hubURL]
	if !ok {
		h = &HubStatus{HubURL: hubURL, State: StateIdle}
		b.hubs[hubURL] = h
	}
	fn(h)
	h.HubURL = hubURL
	h.UpdatedAt = b.now().UTC()
	b.publishLocked()
}

// Remove drops hubURL from the board and notifies subscribers.
func (b *Board) Remove(hubURL string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.hubs[hubURL]; !ok {
		return
	}
	delete(b.hubs, hubURL)
	b.publishLocked()
}

// Snapshot returns a copy of all hub statuses, sorted by hub URL.
func (b *Board) Snapshot() []HubStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snapshotLocked()
}

// Subscribe returns a channel that receives the latest snapshot after every change (and once
// immediately), plus a function that unsubscribes and closes the channel.
func (b *Board) Subscribe() (<-chan []HubStatus, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID
	b.nextID++
	ch := make(chan []HubStatus, 1)
	ch <- b.snapshotLocked()
	b.subs[id] = ch
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if c, ok := b.subs[id]; ok {
				delete(b.subs, id)
				close(c)
			}
		})
	}
}

func (b *Board) snapshotLocked() []HubStatus {
	out := make([]HubStatus, 0, len(b.hubs))
	for _, h := range b.hubs {
		cp := *h
		if h.NextRetryAt != nil {
			t := *h.NextRetryAt
			cp.NextRetryAt = &t
		}
		if h.ConnectedSince != nil {
			t := *h.ConnectedSince
			cp.ConnectedSince = &t
		}
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].HubURL < out[j].HubURL })
	return out
}

// publishLocked delivers the newest snapshot to every subscriber without blocking: a stale
// undelivered snapshot is replaced by the new one.
func (b *Board) publishLocked() {
	if len(b.subs) == 0 {
		return
	}
	snap := b.snapshotLocked()
	for _, ch := range b.subs {
		select {
		case ch <- snap:
		default:
			select {
			case <-ch:
			default:
			}
			ch <- snap
		}
	}
}

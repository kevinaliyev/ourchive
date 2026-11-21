// Package events provides a lightweight in-memory fan-out pub/sub bus and
// Server-Sent Events (SSE) HTTP handler for pushing live node status, traffic
// throughput, and broadcast updates to connected browser clients.
package events

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Event represents a real-time Server-Sent Event pushed from the Hub to connected browsers.
type Event struct {
	Type string `json:"type"` // "nodes.updated" | "announcements.updated" | "shares.updated" | "acls.updated" | "ping"
	Data any    `json:"data,omitempty"`
}

// Bus is a lightweight fan-out pub/sub hub for SSE subscribers.
type Bus struct {
	mu   sync.RWMutex
	subs map[chan Event]struct{}
}

func NewBus() *Bus {
	return &Bus{subs: map[chan Event]struct{}{}}
}

func (b *Bus) Subscribe() (chan Event, func()) {
	ch := make(chan Event, 32)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
}

func (b *Bus) Publish(evType string, data any) {
	ev := Event{Type: evType, Data: data}
	b.mu.RLock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
			// Drop if a slow client buffer is full; client will catch up on next tick.
		}
	}
	b.mu.RUnlock()
}

func (b *Bus) Count() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// ServeSSE streams Server-Sent Events to an authenticated browser client.
func (b *Bus) ServeSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsub := b.Subscribe()
	defer unsub()

	// Initial hello event so the browser confirms the stream is live immediately.
	fmt.Fprintf(w, "event: connected\ndata: {\"ok\":true}\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			payload, err := json.Marshal(ev.Data)
			if err != nil {
				payload = []byte("{}")
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, string(payload))
			flusher.Flush()
		}
	}
}

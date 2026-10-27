// Package events is a small fan-out bus for progress and error events that
// the API streams to clients over SSE.
package events

import (
	"sync"
	"time"
)

// Event is one notification. Type is a short machine-readable tag such as
// "indexed", "failed", "removed", "scan", "error".
type Event struct {
	Time time.Time `json:"time"`
	Type string    `json:"type"`
	Path string    `json:"path,omitempty"`
	Msg  string    `json:"msg,omitempty"`
}

// Bus delivers events to subscribers. A subscriber that falls behind loses
// events; the count is reported on its next receive so it is never silent.
type Bus struct {
	mu   sync.Mutex
	subs map[*Sub]struct{}
}

// Sub is one subscription.
type Sub struct {
	C       chan Event
	mu      sync.Mutex
	dropped int
}

// Dropped returns and resets the number of events lost since the last call.
func (s *Sub) Dropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.dropped
	s.dropped = 0
	return n
}

func NewBus() *Bus { return &Bus{subs: map[*Sub]struct{}{}} }

// Subscribe registers a subscriber with a buffer of n events.
func (b *Bus) Subscribe(n int) *Sub {
	s := &Sub{C: make(chan Event, n)}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

// Unsubscribe removes s; its channel is not closed, so pending reads just stop.
func (b *Bus) Unsubscribe(s *Sub) {
	b.mu.Lock()
	delete(b.subs, s)
	b.mu.Unlock()
}

// Publish sends e to every subscriber without blocking.
func (b *Bus) Publish(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		select {
		case s.C <- e:
		default:
			s.mu.Lock()
			s.dropped++
			s.mu.Unlock()
		}
	}
}

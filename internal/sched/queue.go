// Package sched holds the indexing work queue and the pause policy.
package sched

import (
	"context"
	"maps"
	"slices"
	"sync"
)

// Queue is a bounded FIFO of paths with de-duplication. A path that is
// re-added while a worker holds it is queued again once the worker calls
// Done, so the same path is never processed twice concurrently.
//
// Add blocks while the queue is full, which pushes back on the crawler.
// Next blocks while the queue is empty or paused.
type Queue struct {
	mu       sync.Mutex
	cond     *sync.Cond
	cap      int
	items    []string
	queued   map[string]bool
	inflight map[string]bool
	again    map[string]bool // re-added while inflight
	paused   map[string]bool // reasons
	closed   bool
	onPause  func(reasons []string)
}

// NewQueue makes a queue holding at most capacity waiting paths.
func NewQueue(capacity int) *Queue {
	if capacity < 1 {
		panic("sched: queue capacity must be ≥ 1")
	}
	q := &Queue{cap: capacity, queued: map[string]bool{}, inflight: map[string]bool{},
		again: map[string]bool{}, paused: map[string]bool{}}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// wake unblocks waiters when ctx ends; the returned func stops the watcher.
func (q *Queue) wake(ctx context.Context) func() bool {
	return context.AfterFunc(ctx, func() {
		q.mu.Lock()
		q.cond.Broadcast()
		q.mu.Unlock()
	})
}

// Add enqueues path unless it is already waiting. It blocks while full.
func (q *Queue) Add(ctx context.Context, path string) error {
	stop := q.wake(ctx)
	defer stop()
	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		switch {
		case q.closed:
			return context.Canceled
		case ctx.Err() != nil:
			return ctx.Err()
		case q.queued[path]:
			return nil
		case q.inflight[path]:
			q.again[path] = true
			return nil
		case len(q.items) < q.cap:
			q.items = append(q.items, path)
			q.queued[path] = true
			q.cond.Broadcast()
			return nil
		}
		q.cond.Wait()
	}
}

// Next takes the oldest path. The caller must call Done(path) afterwards.
func (q *Queue) Next(ctx context.Context) (string, error) {
	stop := q.wake(ctx)
	defer stop()
	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		switch {
		case q.closed:
			return "", context.Canceled
		case ctx.Err() != nil:
			return "", ctx.Err()
		case len(q.items) > 0 && len(q.paused) == 0:
			p := q.items[0]
			q.items[0] = ""
			q.items = q.items[1:]
			delete(q.queued, p)
			q.inflight[p] = true
			q.cond.Broadcast()
			return p, nil
		}
		q.cond.Wait()
	}
}

// Done releases path. If it was re-added meanwhile, it is queued again
// (exceeding capacity by at most the number of workers, so Done never blocks).
func (q *Queue) Done(path string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.inflight, path)
	if q.again[path] {
		delete(q.again, path)
		if !q.queued[path] {
			q.items = append(q.items, path)
			q.queued[path] = true
		}
	}
	q.cond.Broadcast()
}

// OnPause sets fn to be called with the active reasons (sorted, empty when
// running) each time the set of pause reasons changes. fn runs on the
// goroutine that called Pause or Resume, without the queue's lock.
func (q *Queue) OnPause(fn func(reasons []string)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.onPause = fn
}

// Pause stops Next from handing out work until every reason is resumed.
func (q *Queue) Pause(reason string) {
	q.setPaused(reason, true)
}

// Resume clears one pause reason.
func (q *Queue) Resume(reason string) {
	q.setPaused(reason, false)
}

func (q *Queue) setPaused(reason string, on bool) {
	q.mu.Lock()
	if q.paused[reason] == on {
		q.mu.Unlock()
		return
	}
	if on {
		q.paused[reason] = true
	} else {
		delete(q.paused, reason)
		q.cond.Broadcast()
	}
	fn, reasons := q.onPause, slices.Sorted(maps.Keys(q.paused))
	q.mu.Unlock()
	if fn != nil {
		fn(reasons)
	}
}

// PausedReasons lists active pause reasons, sorted.
func (q *Queue) PausedReasons() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Sorted(maps.Keys(q.paused))
}

// Len returns the number of waiting plus in-flight paths.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items) + len(q.inflight)
}

// Clear drops all waiting paths (in-flight work finishes normally).
func (q *Queue) Clear() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = nil
	clear(q.queued)
	clear(q.again)
	q.cond.Broadcast()
}

// Close wakes every waiter with an error.
func (q *Queue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.cond.Broadcast()
}

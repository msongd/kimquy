package engine

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/msongd/kimquy/internal/events"
)

// slowFileAfter is how long one file may take before it is reported as slow.
var slowFileAfter = 5 * time.Minute // var for tests

// Job is a file a worker is processing, as reported by Status.
type Job struct {
	Path     string    `json:"path"`
	Started  time.Time `json:"started"`
	ElapsedS int64     `json:"elapsed_s"`
	Done     int64     `json:"done"`  // vectors embedded so far
	Total    int64     `json:"total"` // vectors expected; 0 while unknown
}

// job tracks one file in flight. Only done and total change after start.
type job struct {
	path    string
	started time.Time
	done    atomic.Int64
	total   atomic.Int64
	slow    *time.Timer
}

type jobKey struct{}

// startJob registers path as in flight and returns ctx carrying the job, so
// the pipeline can report progress. endJob must follow.
func (e *Engine) startJob(ctx context.Context, path string) (context.Context, *job) {
	j := &job{path: path, started: time.Now()}
	e.jobsMu.Lock()
	if _, dup := e.jobs[path]; dup {
		e.jobsMu.Unlock()
		panic("engine: path processed twice concurrently: " + path) // the queue forbids it
	}
	e.jobs[path] = j
	e.jobsMu.Unlock()
	j.slow = time.AfterFunc(slowFileAfter, func() {
		took, prog := time.Since(j.started).Round(time.Second), j.progress()
		e.log.Warn("slow file", "path", path, "elapsed", took, "progress", prog)
		e.bus.Publish(events.Event{Type: "slow", Path: path, Msg: fmt.Sprintf("still indexing after %v (%s)", took, prog)})
	})
	return context.WithValue(ctx, jobKey{}, j), j
}

func (e *Engine) endJob(j *job) {
	j.slow.Stop()
	e.jobsMu.Lock()
	delete(e.jobs, j.path)
	e.jobsMu.Unlock()
}

// working lists the jobs in flight, oldest first.
func (e *Engine) working() []Job {
	now := time.Now()
	e.jobsMu.Lock()
	out := make([]Job, 0, len(e.jobs))
	for _, j := range e.jobs {
		out = append(out, Job{Path: j.path, Started: j.started, ElapsedS: int64(now.Sub(j.started) / time.Second),
			Done: j.done.Load(), Total: j.total.Load()})
	}
	e.jobsMu.Unlock()
	slices.SortFunc(out, func(a, b Job) int { return a.Started.Compare(b.Started) })
	return out
}

func (j *job) progress() string {
	done, total := j.done.Load(), j.total.Load()
	if total > 0 {
		return fmt.Sprintf("%d/%d vectors", done, total)
	}
	return fmt.Sprintf("%d vectors", done)
}

// progressDone adds n embedded vectors to the job in ctx, if any.
func progressDone(ctx context.Context, n int) {
	if j, ok := ctx.Value(jobKey{}).(*job); ok {
		j.done.Add(int64(n))
	}
}

// progressExpect adds n vectors to the total expected for the job in ctx.
func progressExpect(ctx context.Context, n int) {
	if j, ok := ctx.Value(jobKey{}).(*job); ok {
		j.total.Add(int64(n))
	}
}

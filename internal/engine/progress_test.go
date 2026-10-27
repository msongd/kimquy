package engine

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/msongd/kimquy/internal/embed"
)

// gated embeds the first indexing batch, then blocks until release closes.
type gated struct {
	embed.Fake
	calls   atomic.Int32
	release chan struct{}
}

func (g *gated) EmbedTexts(ctx context.Context, p embed.Priority, texts []string) ([][]float32, error) {
	if p == embed.Index && g.calls.Add(1) > 1 {
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.Fake.EmbedTexts(ctx, p, texts)
}

func TestWorkingAndSlowFile(t *testing.T) {
	old := slowFileAfter
	slowFileAfter = 50 * time.Millisecond
	t.Cleanup(func() { slowFileAfter = old })

	root, state := t.TempDir(), t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	big := filepath.Join(root, "big.log")
	write(t, big, strings.Repeat("alpha beta gamma delta ", 20000)) // several batches
	g := &gated{Fake: embed.Fake{Dims: 768}, release: make(chan struct{})}
	h := start(t, state, root, g)
	if _, err := h.e.SetRoots([]string{root}, nil); err != nil {
		t.Fatal(err)
	}

	var job Job
	until(t, 10*time.Second, "file in flight with progress", func() bool {
		w := h.e.Status().Working
		if len(w) != 1 || w[0].Done == 0 {
			return false
		}
		job = w[0]
		return true
	})
	if job.Path != big || job.Total <= job.Done || job.Started.IsZero() {
		t.Fatalf("working %+v", job)
	}
	for ev := range h.sub.C {
		if ev.Type == "slow" {
			if ev.Path != big || !strings.Contains(ev.Msg, "vectors") {
				t.Fatalf("slow event %+v", ev)
			}
			break
		}
	}

	close(g.release)
	for ev := range h.sub.C {
		if ev.Type == "indexed" {
			if ev.Path != big || !strings.Contains(ev.Msg, " vectors in ") {
				t.Fatalf("indexed event %+v", ev)
			}
			break
		}
	}
	h.idle()
	if w := h.e.Status().Working; len(w) != 0 {
		t.Fatalf("still working after idle: %+v", w)
	}
	h.stop()
}

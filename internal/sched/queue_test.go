package sched

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestQueueDedupAndOrder(t *testing.T) {
	ctx := context.Background()
	q := NewQueue(10)
	for _, p := range []string{"a", "b", "a", "c"} {
		q.Add(ctx, p)
	}
	if q.Len() != 3 {
		t.Fatalf("len %d", q.Len())
	}
	p, _ := q.Next(ctx)
	if p != "a" {
		t.Fatalf("got %q", p)
	}
	q.Add(ctx, "a") // re-added while in flight
	if q.Len() != 3 {
		t.Fatalf("in-flight re-add changed len: %d", q.Len())
	}
	q.Done("a")
	for _, want := range []string{"b", "c", "a"} {
		p, _ := q.Next(ctx)
		if p != want {
			t.Fatalf("got %q want %q", p, want)
		}
		q.Done(p)
	}
}

func TestQueueBackpressureAndPause(t *testing.T) {
	ctx := context.Background()
	q := NewQueue(1)
	q.Add(ctx, "a")
	added := make(chan struct{})
	go func() { q.Add(ctx, "b"); close(added) }()
	select {
	case <-added:
		t.Fatal("Add did not block on a full queue")
	case <-time.After(50 * time.Millisecond):
	}
	q.Pause("battery")
	tctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := q.Next(tctx); err == nil {
		t.Fatal("Next returned while paused")
	}
	q.Resume("battery")
	p, err := q.Next(ctx)
	if err != nil || p != "a" {
		t.Fatalf("Next = %q, %v", p, err)
	}
	<-added
	q.Done(p)
	q.Close()
	if _, err := q.Next(ctx); err == nil {
		t.Fatal("Next after Close succeeded")
	}
}

func TestQueueOnPause(t *testing.T) {
	q := NewQueue(1)
	var got []string
	q.OnPause(func(r []string) { got = append(got, strings.Join(r, "+")) })
	q.Pause("b")
	q.Pause("a")
	q.Pause("a") // no change
	q.Resume("b")
	q.Resume("x") // no change
	q.Resume("a")
	if want := []string{"b", "a+b", "a", ""}; !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

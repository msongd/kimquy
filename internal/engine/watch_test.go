package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/msongd/kimquy/internal/embed"
	"github.com/msongd/kimquy/internal/events"
	"github.com/msongd/kimquy/internal/sched"
	"github.com/msongd/kimquy/internal/watch"
)

// fakeWatch is a watch.System whose events the test sends by hand.
type fakeWatch struct {
	mu      sync.Mutex
	now     uint64
	history map[string]string // root → UUID; missing means "same for all"
	starts  []uint64          // since of each Watch call
	stream  *fakeStream
}

type fakeStream struct {
	mu       sync.Mutex
	evs      []watch.Event
	overflow bool
	ready    chan struct{}
	closed   bool
}

func (f *fakeWatch) Now() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeWatch) History(path string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, ok := f.history[path]; ok {
		return u, nil
	}
	return "uuid-1", nil
}

func (f *fakeWatch) Watch(dirs []string, since uint64) (watch.Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, since)
	f.stream = &fakeStream{ready: make(chan struct{}, 1)}
	return f.stream, nil
}

// send delivers events with fresh IDs on the current stream.
func (f *fakeWatch) send(t *testing.T, overflow bool, evs ...watch.Event) {
	t.Helper()
	f.mu.Lock()
	s := f.stream
	for i := range evs {
		f.now++
		evs[i].ID = f.now
	}
	f.mu.Unlock()
	if s == nil {
		t.Fatal("no stream")
	}
	s.mu.Lock()
	s.evs = append(s.evs, evs...)
	s.overflow = s.overflow || overflow
	s.mu.Unlock()
	select {
	case s.ready <- struct{}{}:
	default:
	}
}

// advance moves the event clock, as other activity on the host would.
func (f *fakeWatch) advance(n uint64) {
	f.mu.Lock()
	f.now += n
	f.mu.Unlock()
}

func (f *fakeWatch) lastStart() (uint64, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.starts) == 0 {
		return 0, 0
	}
	return f.starts[len(f.starts)-1], len(f.starts)
}

func (s *fakeStream) Ready() <-chan struct{} { return s.ready }
func (s *fakeStream) Take() ([]watch.Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	evs, o := s.evs, s.overflow
	s.evs, s.overflow = nil, false
	return evs, o
}
func (s *fakeStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func startWatching(t *testing.T, state, root string, emb embed.Embedder, sys watch.System) *harness {
	t.Helper()
	bus := events.NewBus()
	e, err := Open(Options{StateDir: state, Embedder: emb, Bus: bus, Watch: sys})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, state: state, root: root, e: e, sub: bus.Subscribe(4096), done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- e.Run(ctx) }()
	return h
}

// until polls cond for up to d.
func until(t *testing.T, d time.Duration, what string, cond func() bool) time.Duration {
	t.Helper()
	t0 := time.Now()
	for !cond() {
		if time.Since(t0) > d {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return time.Since(t0)
}

// found reports whether path is a fresh hit for q whose text contains q's
// first word. The fake embedder ranks every file, so a bare path match
// would accept a non-match in a small corpus.
func (h *harness) found(q, path string) bool {
	h.t.Helper()
	word := strings.Fields(q)[0]
	for _, hit := range h.search(q, 5) {
		if hit.Path == path && !hit.Stale && strings.Contains(strings.ToLower(hit.Snippet), word) {
			return true
		}
	}
	return false
}

func (h *harness) gone(path string) bool {
	_, ok := h.e.cat.Lookup(path)
	return !ok
}

func readWatchState(t *testing.T, state string) watchState {
	t.Helper()
	var s watchState
	data, err := os.ReadFile(filepath.Join(state, "watch.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func fastCheckpoints(t *testing.T) {
	old := checkpointEvery
	checkpointEvery = 20 * time.Millisecond
	t.Cleanup(func() { checkpointEvery = old })
}

func TestWatchEventsAndResume(t *testing.T) {
	fastCheckpoints(t)
	root, state := t.TempDir(), t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	a := filepath.Join(root, "a.txt")
	write(t, a, "Otters hold hands while sleeping.")
	sys := &fakeWatch{now: 100}

	h := startWatching(t, state, root, &embed.Fake{Dims: 768}, sys)
	if st := h.e.Status(); st.Watch != watchIdle && st.Watch != watchOff {
		t.Fatalf("watch status before roots: %q", st.Watch)
	}
	if _, err := h.e.SetRoots([]string{root}, nil); err != nil {
		t.Fatal(err)
	}
	until(t, 5*time.Second, "stream start", func() bool { _, n := sys.lastStart(); return n == 1 })
	if since, _ := sys.lastStart(); since != 100 {
		t.Fatalf("first watch since %d, want Now()=100", since)
	}
	h.idle()
	if !h.found("otters sleeping", a) || h.e.Status().Watch != watchLive {
		t.Fatalf("initial scan: %+v", h.e.Status())
	}

	// A new file and an edit arrive as events, without any scan.
	scans := h.e.scanReqN.Load()
	b := filepath.Join(root, "b.md")
	write(t, b, "Volcanoes erupt molten lava.")
	write(t, a, "Hedgehogs roll into spiky balls.")
	sys.send(t, false,
		watch.Event{Path: b, Flags: watch.Created | watch.IsFile},
		watch.Event{Path: a, Flags: watch.Modified | watch.IsFile},
		watch.Event{Path: filepath.Join(root, "ignored.bin"), Flags: watch.Created | watch.IsFile})
	until(t, 5*time.Second, "events indexed", func() bool {
		return h.found("volcanoes lava", b) && h.found("hedgehogs spiky", a)
	})
	if h.e.scanReqN.Load() != scans {
		t.Fatal("file events caused a full scan")
	}

	// A directory appearing is scanned on its own.
	sub := filepath.Join(root, "sub")
	c := filepath.Join(sub, "deep/c.txt")
	write(t, c, "Glaciers carve valleys slowly.")
	sys.send(t, false, watch.Event{Path: sub, Flags: watch.Created | watch.IsDir})
	until(t, 5*time.Second, "new dir indexed", func() bool { return h.found("glaciers valleys", c) })
	// … and disappearing.
	os.RemoveAll(sub)
	sys.send(t, false, watch.Event{Path: sub, Flags: watch.Removed | watch.IsDir})
	until(t, 5*time.Second, "removed dir dropped", func() bool { return h.gone(c) })
	if h.e.scanReqN.Load() != scans {
		t.Fatal("dir events caused a full scan")
	}

	// Checkpoint covers everything handled.
	last := sys.Now()
	until(t, 5*time.Second, "checkpoint", func() bool {
		data, _ := os.ReadFile(filepath.Join(state, "watch.json"))
		var s watchState
		return json.Unmarshal(data, &s) == nil && s.EventID == last
	})
	h.stop()

	// Offline change, then restart: replay from the checkpoint, no full scan.
	sys.advance(50)
	d := filepath.Join(root, "d.txt")
	write(t, d, "Lighthouses guide ships at night.")
	h = startWatching(t, state, root, &embed.Fake{Dims: 768}, sys)
	until(t, 5*time.Second, "stream start", func() bool { _, n := sys.lastStart(); return n == 2 })
	if since, _ := sys.lastStart(); since != last {
		t.Fatalf("resumed since %d, want %d", since, last)
	}
	if h.e.Status().Watch != watchReplaying {
		t.Fatalf("status %q, want replaying", h.e.Status().Watch)
	}
	sys.send(t, false, watch.Event{Path: d, Flags: watch.Created | watch.IsFile}, watch.Event{Flags: watch.HistoryDone})
	until(t, 5*time.Second, "replayed file indexed", func() bool { return h.found("lighthouses ships", d) })
	until(t, 5*time.Second, "live", func() bool { return h.e.Status().Watch == watchLive })
	if n := h.e.scanReqN.Load(); n != 0 {
		t.Fatalf("restart requested %d full scans", n)
	}
	if !h.found("hedgehogs spiky", a) {
		t.Fatal("old results lost")
	}

	// Lost events force a full scan.
	sys.send(t, false, watch.Event{Path: root, Flags: watch.KernelDropped})
	until(t, 5*time.Second, "full scan", func() bool { return h.e.scanDoneN.Load() == 1 })
	// So does an overflowing buffer.
	sys.send(t, true)
	until(t, 5*time.Second, "full scan", func() bool { return h.e.scanDoneN.Load() == 2 })
	h.stop()

	// A different event history (e.g. the volume was erased) cannot be replayed.
	sys.history = map[string]string{root: "uuid-2"}
	h = startWatching(t, state, root, &embed.Fake{Dims: 768}, sys)
	until(t, 5*time.Second, "full scan", func() bool { return h.e.scanDoneN.Load() == 1 })
	if since, _ := sys.lastStart(); since != sys.Now() {
		t.Fatalf("watch since %d, want now %d", since, sys.Now())
	}
	h.idle()
	until(t, 5*time.Second, "checkpoint", func() bool { return readWatchState(t, state).History[root] == "uuid-2" })
	h.stop()
}

// The replay point must not pass changes that are queued but not indexed.
func TestWatchCheckpointWaitsForWork(t *testing.T) {
	fastCheckpoints(t)
	root, state := t.TempDir(), t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	sys := &fakeWatch{now: 10}
	h := startWatching(t, state, root, &embed.Fake{Dims: 768}, sys)
	if _, err := h.e.SetRoots([]string{root}, nil); err != nil {
		t.Fatal(err)
	}
	h.idle()
	until(t, 5*time.Second, "checkpoint", func() bool {
		_, err := os.Stat(filepath.Join(state, "watch.json"))
		return err == nil
	})
	h.e.Pause()
	p := filepath.Join(root, "x.txt")
	write(t, p, "Puffins nest on cliffs.")
	sys.send(t, false, watch.Event{Path: p, Flags: watch.Created | watch.IsFile})
	until(t, 5*time.Second, "queued", func() bool { return h.e.q.Len() == 1 })
	time.Sleep(10 * checkpointEvery)
	if s := readWatchState(t, state); s.EventID != 10 {
		t.Fatalf("checkpoint advanced to %d with work pending", s.EventID)
	}
	h.e.Resume()
	until(t, 5*time.Second, "checkpoint", func() bool { return readWatchState(t, state).EventID == 11 })
	h.stop()
}

// Changing roots restarts the stream and scans; nothing is replayed.
func TestWatchRootsChange(t *testing.T) {
	fastCheckpoints(t)
	r1, r2, state := t.TempDir(), t.TempDir(), t.TempDir()
	r1, _ = filepath.EvalSymlinks(r1)
	r2, _ = filepath.EvalSymlinks(r2)
	write(t, filepath.Join(r1, "one.txt"), "Pelicans scoop fish.")
	write(t, filepath.Join(r2, "two.txt"), "Cacti store water.")
	sys := &fakeWatch{now: 1}
	h := startWatching(t, state, r1, &embed.Fake{Dims: 768}, sys)
	if _, err := h.e.SetRoots([]string{r1}, nil); err != nil {
		t.Fatal(err)
	}
	h.idle()
	sys.advance(5)
	if _, err := h.e.SetRoots([]string{r2}, nil); err != nil {
		t.Fatal(err)
	}
	until(t, 5*time.Second, "restart", func() bool { _, n := sys.lastStart(); return n == 2 })
	h.idle()
	if !h.gone(filepath.Join(r1, "one.txt")) || !h.found("cacti water", filepath.Join(r2, "two.txt")) {
		t.Fatalf("after root change: %+v", h.search("pelicans cacti", 5))
	}
	until(t, 5*time.Second, "checkpoint", func() bool {
		return slices.Equal(readWatchState(t, state).Roots, []string{r2})
	})
	h.stop()
}

func TestOutermost(t *testing.T) {
	got := outermost([]string{"/a/b", "/a", "/c/d", "/a/b/c", "/c/d", "/cd"})
	if want := []string{"/a", "/c/d", "/cd"}; !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
}

// Exit criterion for M5 with real FSEvents: an edit is searchable within
// 5 s, and a restart replays changes instead of rescanning.
func TestWatchFSEvents(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("needs macOS")
	}
	sys, err := watch.Native()
	if err != nil {
		t.Fatal(err)
	}
	fastCheckpoints(t)
	root, state := t.TempDir(), t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	a := filepath.Join(root, "notes/a.txt")
	write(t, a, "Koalas eat eucalyptus leaves.")
	h := startWatching(t, state, root, &embed.Fake{Dims: 768}, sys)
	if _, err := h.e.SetRoots([]string{root}, nil); err != nil {
		t.Fatal(err)
	}
	h.idle()
	if !h.found("koalas eucalyptus", a) {
		t.Fatal("initial scan")
	}

	write(t, a, "Narwhals have a long spiral tusk.")
	lat := until(t, 5*time.Second, "edit searchable", func() bool { return h.found("narwhals tusk", a) })
	t.Logf("edit searchable after %v", lat.Round(time.Millisecond))
	b := filepath.Join(root, "new/dir/b.md")
	write(t, b, "Flamingos stand on one leg.")
	lat = until(t, 5*time.Second, "new file searchable", func() bool { return h.found("flamingos leg", b) })
	t.Logf("file in new dir searchable after %v", lat.Round(time.Millisecond))
	os.Remove(a)
	lat = until(t, 5*time.Second, "delete applied", func() bool { return h.gone(a) })
	t.Logf("delete applied after %v", lat.Round(time.Millisecond))
	h.idle()
	time.Sleep(10 * checkpointEvery)
	h.stop()

	c := filepath.Join(root, "offline.txt")
	write(t, c, "Wombats produce cube shaped droppings.")
	write(t, b, "Penguins propose with pebbles.")
	time.Sleep(time.Second) // let fseventsd record it
	h = startWatching(t, state, root, &embed.Fake{Dims: 768}, sys)
	lat = until(t, 10*time.Second, "offline changes indexed", func() bool {
		return h.found("wombats cube", c) && h.found("penguins pebbles", b)
	})
	t.Logf("offline changes indexed %v after restart", lat.Round(time.Millisecond))
	until(t, 10*time.Second, "replay done", func() bool { return h.e.Status().Watch == watchLive })
	if n := h.e.scanReqN.Load(); n != 0 {
		t.Fatalf("restart requested %d full scans", n)
	}
	h.stop()
}

func TestPowerPausesIndexing(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	p := filepath.Join(root, "p.txt")
	write(t, p, "Seahorse fathers carry the eggs.")
	var mu sync.Mutex
	pw := sched.Power{OnBattery: true}
	e, err := Open(Options{StateDir: state, Embedder: &embed.Fake{Dims: 768}, PowerEvery: 5 * time.Millisecond,
		Power: func() (sched.Power, error) { mu.Lock(); defer mu.Unlock(); return pw, nil }})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, state: state, root: root, e: e, sub: events.NewBus().Subscribe(1), done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- e.Run(ctx) }()
	if _, err := e.SetRoots([]string{root}, nil); err != nil {
		t.Fatal(err)
	}
	until(t, 5*time.Second, "paused", func() bool { return slices.Contains(e.Status().Paused, sched.PauseBattery) })
	until(t, 5*time.Second, "queued", func() bool { return e.q.Len() == 1 })
	time.Sleep(50 * time.Millisecond)
	if e.Status().Indexed != 0 {
		t.Fatal("indexed on battery")
	}
	mu.Lock()
	pw = sched.Power{}
	mu.Unlock()
	until(t, 5*time.Second, "indexed on AC", func() bool { return h.found("seahorse eggs", p) })
	if len(e.Status().Paused) != 0 {
		t.Fatalf("still paused: %v", e.Status().Paused)
	}
	h.stop()
}

// A root that cannot be read (no Full Disk Access yet) is tried again, and
// no replay point is saved meanwhile: a restart must scan it in full.
func TestUnreadableRootRetried(t *testing.T) {
	fastCheckpoints(t)
	old := badRootEvery
	badRootEvery = 50 * time.Millisecond
	t.Cleanup(func() { badRootEvery = old })
	base, state := t.TempDir(), t.TempDir()
	base, _ = filepath.EvalSymlinks(base)
	good, locked := filepath.Join(base, "good"), filepath.Join(base, "locked")
	a, b := filepath.Join(good, "a.txt"), filepath.Join(locked, "b.txt")
	write(t, a, "Otters hold hands while sleeping.")
	write(t, b, "Lighthouses guide ships at night.")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })

	sys := &fakeWatch{now: 10}
	h := startWatching(t, state, base, &embed.Fake{Dims: 768}, sys)
	if _, err := h.e.SetRoots([]string{good, locked}, nil); err != nil {
		t.Fatal(err)
	}
	h.idle()
	until(t, 5*time.Second, "root error", func() bool {
		for _, r := range h.e.Status().Roots {
			if r.Path == locked && r.Error != "" {
				return true
			}
		}
		return false
	})
	if !h.found("otters sleeping", a) {
		t.Fatal("readable root not indexed")
	}
	time.Sleep(10 * checkpointEvery)
	if _, err := os.Stat(filepath.Join(state, "watch.json")); !os.IsNotExist(err) {
		t.Fatalf("replay point saved with an unreadable root: %v", err)
	}

	os.Chmod(locked, 0o700) // access granted: no event says so
	until(t, 5*time.Second, "retried root indexed", func() bool { return h.found("lighthouses ships", b) })
	for _, r := range h.e.Status().Roots {
		if r.Error != "" {
			t.Fatalf("root error remains: %+v", r)
		}
	}
	until(t, 5*time.Second, "checkpoint", func() bool {
		_, err := os.Stat(filepath.Join(state, "watch.json"))
		return err == nil
	})
	h.stop()
}

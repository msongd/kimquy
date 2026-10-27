package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/msongd/kimquy/internal/agent"
	"github.com/msongd/kimquy/internal/client"
	"github.com/msongd/kimquy/internal/events"
	"github.com/msongd/kimquy/internal/kind"
)

// fakeSemd answers like semd from fields the test sets, and records calls.
type fakeSemd struct {
	hits     map[string][]client.Hit // by query
	searches []string
	reqs     []client.SearchRequest
	status   client.Status
	statuses int // Status calls
	// stream answers Events (from another goroutine); nil fails it.
	stream   func(context.Context, func(events.Event)) error
	down     error // every call fails with it
	roots    client.RootsResponse
	setRoots []client.RootsRequest
	settings client.Settings
	failures []client.Failure
	paused   int
	resumed  int
	reindex  int
}

func (f *fakeSemd) Search(_ context.Context, r client.SearchRequest) (client.SearchResponse, error) {
	f.searches = append(f.searches, r.Q)
	f.reqs = append(f.reqs, r)
	return client.SearchResponse{Hits: f.hits[r.Q], TookMS: 7}, f.down
}
func (f *fakeSemd) Status(context.Context) (client.Status, error) {
	f.statuses++
	return f.status, f.down
}
func (f *fakeSemd) Events(ctx context.Context, fn func(events.Event)) error {
	if f.stream == nil {
		return errors.New("no stream")
	}
	return f.stream(ctx, fn)
}
func (f *fakeSemd) Roots(context.Context) (client.RootsResponse, error) {
	return f.roots, f.down
}
func (f *fakeSemd) SetRoots(_ context.Context, r client.RootsRequest) (client.RootsResponse, error) {
	if f.down != nil {
		return client.RootsResponse{}, f.down
	}
	f.setRoots = append(f.setRoots, r)
	f.roots.Roots = r.Roots
	if r.Excludes != nil {
		f.roots.Excludes = r.Excludes
	}
	return f.roots, nil
}
func (f *fakeSemd) Failures(context.Context) ([]client.Failure, error) { return f.failures, f.down }
func (f *fakeSemd) Settings(context.Context) (client.Settings, error)  { return f.settings, f.down }
func (f *fakeSemd) SetSettings(_ context.Context, s client.Settings) (client.Settings, error) {
	if f.down != nil {
		return client.Settings{}, f.down
	}
	f.settings = s
	return s, nil
}
func (f *fakeSemd) Pause(context.Context) error   { f.paused++; return f.down }
func (f *fakeSemd) Resume(context.Context) error  { f.resumed++; return f.down }
func (f *fakeSemd) Reindex(context.Context) error { f.reindex++; return f.down }

type fakeShell struct {
	opened, revealed []string
	pick             []string
	pickModel        string
	privacy          int
	hidden           int
	quit             int
}

func (s *fakeShell) Open(p string) error            { s.opened = append(s.opened, p); return nil }
func (s *fakeShell) Reveal(p string) error          { s.revealed = append(s.revealed, p); return nil }
func (s *fakeShell) PickFolders() ([]string, error) { return s.pick, nil }
func (s *fakeShell) PickModel() (string, error)     { return s.pickModel, nil }
func (s *fakeShell) OpenPrivacySettings() error     { s.privacy++; return nil }
func (s *fakeShell) HideWindow()                    { s.hidden++ }
func (s *fakeShell) Quit()                          { s.quit++ }

// fakeService answers like the semd LaunchAgent, and records calls.
type fakeService struct {
	info         agent.Info
	fail         error // every action fails with it
	starts       int
	stops        int
	restarts     int
	uninstalls   int
	removed      []string
	uninstallErr error
}

func (s *fakeService) Label() string { return "dev.kimquy.test" }
func (s *fakeService) LogPath() string {
	return "/Users/me/Library/Logs/Kim Quy/dev.kimquy.test.log"
}
func (s *fakeService) Info() (agent.Info, error) { return s.info, nil }
func (s *fakeService) Start() error {
	s.starts++
	if s.fail == nil {
		s.info = agent.Info{Loaded: true, State: "running", PID: 42, Runs: 1, LastExit: "(never exited)", Program: "/x/semd"}
	}
	return s.fail
}
func (s *fakeService) Stop() error {
	s.stops++
	if s.fail == nil {
		s.info = agent.Info{}
	}
	return s.fail
}
func (s *fakeService) Restart() error { s.restarts++; return s.fail }
func (s *fakeService) Uninstall() ([]string, error) {
	s.uninstalls++
	return s.removed, s.uninstallErr
}

// harness runs the app in a Tester; async work waits in a queue until
// flush, as replies from semd come later than the frame that asked.
type harness struct {
	*ui.Tester
	t      *testing.T
	a      *app
	semd   *fakeSemd
	shell  *fakeShell
	queue  []func() func()
	builds int // of the view
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, semd: &fakeSemd{hits: map[string][]client.Hit{}}}
	return h.start(h.semd)
}

func (h *harness) start(be backend) *harness {
	h.shell = &fakeShell{}
	h.a = newApp(be, h.shell, func(w func() func()) { h.queue = append(h.queue, w) })
	h.Tester = ui.NewTester(func(c *ui.Context) { h.builds++; h.a.view(c) }, 760, 900)
	h.Frame()
	h.flush()
	return h
}

// flush runs the queued work and applies its results, then draws.
func (h *harness) flush() {
	h.t.Helper()
	for len(h.queue) > 0 {
		w := h.queue[0]
		h.queue = h.queue[1:]
		if apply := w(); apply != nil {
			apply()
		}
		h.Frame()
	}
	h.Frame()
}

func (h *harness) want(s string) {
	h.t.Helper()
	if !h.HasText(s) {
		h.t.Fatalf("no %q in %q", s, h.Texts())
	}
}

func (h *harness) click(s string) {
	h.t.Helper()
	if err := h.Click(s); err != nil {
		h.t.Fatalf("click %q: %v in %q", s, err, h.Texts())
	}
	h.Frame()
}

func init() { home = func() string { return "/Users/me" } }

var (
	pdfHit = client.Hit{Path: "/Users/me/Docs/report.pdf", Kind: "pdf", Score: .81,
		Locator: kind.Locator{Kind: kind.LocPage, Page: 3}, Snippet: "quarterly\nrevenue  grew"}
	talkHit = client.Hit{Path: "/Users/me/Talks/keynote.m4a", Kind: "audio", Score: .62,
		Locator: kind.Locator{Kind: kind.LocTime, Start: 65_000, End: 3_725_000}, Stale: true}
)

func TestSearchDebouncedAndShown(t *testing.T) {
	h := newHarness(t)
	h.semd.hits["revenue"] = []client.Hit{pdfHit, talkHit}
	h.Type("revenue")
	h.Frame()
	h.flush()
	if len(h.semd.searches) != 0 {
		t.Fatalf("searched before the pause: %v", h.semd.searches)
	}
	h.Advance(debounce)
	h.flush()
	if !slices.Equal(h.semd.searches, []string{"revenue"}) {
		t.Fatalf("searches %v", h.semd.searches)
	}
	for _, s := range []string{"report.pdf", "~/Docs · page 3", "quarterly revenue grew", "0.81",
		"keynote.m4a  (changed since indexed)", "~/Talks · 1:05–1:02:05", "PDF", "Audio", "2 results in 7 ms"} {
		h.want(s)
	}

	h.Key(0, ui.KeyEnter)
	h.Frame()
	if !slices.Equal(h.shell.opened, []string{pdfHit.Path}) {
		t.Fatalf("Enter opened %v", h.shell.opened)
	}
	h.Key(0, ui.KeyDown)
	h.Key(ui.Cmd, ui.KeyEnter)
	h.Frame()
	if !slices.Equal(h.shell.revealed, []string{talkHit.Path}) || len(h.shell.opened) != 1 {
		t.Fatalf("Cmd-Enter: revealed %v, opened %v", h.shell.revealed, h.shell.opened)
	}

	h.Key(0, ui.KeyEscape) // clears
	h.Frame()
	h.want("Type to search the contents of your files.")
}

// Typing on resets the pause: one search, for the whole query.
func TestSearchWaitsForPause(t *testing.T) {
	h := newHarness(t)
	for _, r := range "budget" {
		h.Type(string(r))
		h.Advance(debounce / 2)
		h.flush()
	}
	h.Advance(debounce)
	h.flush()
	if !slices.Equal(h.semd.searches, []string{"budget"}) {
		t.Fatalf("searches %v", h.semd.searches)
	}
	h.want("No matches.")
}

// A reply to an older query never replaces a newer one's.
func TestStaleReplyDropped(t *testing.T) {
	h := newHarness(t)
	h.semd.hits["old"] = []client.Hit{talkHit}
	h.semd.hits["new"] = []client.Hit{pdfHit}
	h.a.search("old")
	old := h.queue[len(h.queue)-1]
	h.queue = h.queue[:len(h.queue)-1]
	h.a.search("new")
	h.flush()
	old()() // arrives last
	h.Frame()
	h.want("report.pdf")
	if h.HasText("keynote.m4a  (changed since indexed)") {
		t.Fatal("stale reply shown")
	}
}

func TestSemdDown(t *testing.T) {
	h := newHarness(t)
	h.semd.down = errors.New("dial unix semd.sock: connect: no such file or directory")
	h.Advance(statusEvery)
	h.flush()
	h.want("semd is not reachable (is it running?): dial unix semd.sock: connect: no such file or directory")
	h.want("semd not connected")

	h.semd.down = nil
	h.semd.status = client.Status{Indexed: 1234, Pending: 5, Paused: []string{"on battery"}}
	h.Advance(statusEvery)
	h.flush()
	h.want("1234 indexed · 5 pending · paused (on battery)")
}

func TestSettings(t *testing.T) {
	h := newHarness(t)
	h.semd.roots = client.RootsResponse{Roots: []string{"/Users/me/Docs", "/Volumes/Old"}, Excludes: []string{"*.bak"}, DefaultExcludes: []string{".*"}}
	h.semd.settings = client.Settings{PauseOnBattery: true}
	h.semd.failures = []client.Failure{{Path: "/Users/me/Docs/broken.pdf", Error: "pdf: no pages"}}
	h.semd.status = client.Status{Watch: "live", Roots: []client.RootStatus{{Path: "/Volumes/Old", Error: "open /Volumes/Old: operation not permitted"}}}
	h.Advance(statusEvery)
	h.flush()

	h.click("Settings")
	h.flush()
	for _, s := range []string{"~/Docs", "/Volumes/Old", "Failed (1)"} {
		h.want(s)
	}
	if !strings.Contains(strings.Join(h.Texts(), "\n"), "Full Disk Access (see Privacy)") {
		t.Errorf("no permission hint in %q", h.Texts())
	}
	h.click("Failed (1)")
	h.want("~/Docs/broken.pdf")
	h.want("pdf: no pages")
	h.click("Indexing")
	h.want("Watching: live")
	h.click("Folders")

	if err := h.Click("Remove"); err != nil { // the first: ~/Docs
		t.Fatal(err)
	}
	h.flush()
	if len(h.semd.setRoots) != 1 || !slices.Equal(h.semd.setRoots[0].Roots, []string{"/Volumes/Old"}) || h.semd.setRoots[0].Excludes != nil {
		t.Fatalf("remove sent %+v", h.semd.setRoots)
	}

	h.shell.pick = []string{"/Users/me/Notes", "/Volumes/Old"}
	h.click("Add Folders…")
	h.flush()
	if got := h.semd.setRoots[len(h.semd.setRoots)-1].Roots; !slices.Equal(got, []string{"/Volumes/Old", "/Users/me/Notes"}) {
		t.Fatalf("add sent %v", got)
	}
	h.want("~/Notes")

	// Clearing the excludes sends [], not nil (which would keep them).
	h.click("Excluded")
	h.a.excludes = ""
	h.Frame()
	h.click("Save Exclusions")
	h.flush()
	if got := h.semd.setRoots[len(h.semd.setRoots)-1].Excludes; got == nil || len(got) != 0 {
		t.Fatalf("cleared excludes sent %#v", got)
	}

	h.a.pauseOnBattery = false // as the switch does
	h.a.setPauseOnBattery(false)
	h.flush()
	if h.semd.settings.PauseOnBattery {
		t.Fatal("setting not sent")
	}

	h.click("Indexing")
	h.click("Pause Indexing")
	h.flush()
	if h.semd.paused != 1 {
		t.Fatal("not paused")
	}
	h.click("Rebuild Index…")
	h.click("Rebuild")
	h.flush()
	if h.semd.reindex != 1 {
		t.Fatal("no reindex")
	}
}

func TestSettingsFailureReverts(t *testing.T) {
	h := newHarness(t)
	h.a.settingsOpen, h.a.pauseOnBattery = true, false
	h.semd.down = errors.New("boom")
	h.a.setPauseOnBattery(false)
	h.flush()
	if !h.a.pauseOnBattery {
		t.Fatal("switch not reverted")
	}
	h.want("Cannot change the setting: boom")
}

func TestFormatting(t *testing.T) {
	for ms, want := range map[int64]string{0: "0:00", 59_999: "0:59", 61_000: "1:01", 3_600_000: "1:00:00", -5: "0:00"} {
		if got := clock(ms); got != want {
			t.Errorf("clock(%d) = %q, want %q", ms, got, want)
		}
	}
	for p, want := range map[string]string{"/Users/me": "~", "/Users/me/x": "~/x", "/Users/meow": "/Users/meow", "/tmp": "/tmp"} {
		if got := tildePath(p); got != want {
			t.Errorf("tildePath(%q) = %q, want %q", p, got, want)
		}
	}
	if got := patterns(" a \n\n b/c\n"); !slices.Equal(got, []string{"a", "b/c"}) {
		t.Errorf("patterns %q", got)
	}
	if kindLabel("") != "?" || kindLabel("text") != "Text" || kindLabel("pdf") != "PDF" {
		t.Error("kindLabel")
	}
}

// With a row clicked (the list has the focus), Enter and a double click
// open it once each.
func TestOpenFromList(t *testing.T) {
	h := newHarness(t)
	h.semd.hits["q"] = []client.Hit{pdfHit, talkHit}
	h.Type("q")
	h.Advance(debounce)
	h.flush()
	h.click("~/Talks · 1:05–1:02:05")
	h.Key(0, ui.KeyEnter)
	h.Frame()
	if !slices.Equal(h.shell.opened, []string{talkHit.Path}) {
		t.Fatalf("Enter on list opened %v", h.shell.opened)
	}
	r, ok := h.Find("~/Docs · page 3")
	if !ok {
		t.Fatal("row not found")
	}
	h.ClickAt(r.X+2, r.Y+2)
	h.ClickAt(r.X+2, r.Y+2)
	h.Frame()
	if !slices.Equal(h.shell.opened, []string{talkHit.Path, pdfHit.Path}) {
		t.Fatalf("double click opened %v", h.shell.opened)
	}
}

// TestRealSemd searches a running semd and draws the window, when
// SEMSEARCH_SOCKET names its socket and SEMSEARCH_QUERY the query:
//
//	SEMSEARCH_SOCKET=… SEMSEARCH_QUERY=… SEMSEARCH_SHOT=/abs/prefix go test -run RealSemd -v
func TestRealSemd(t *testing.T) {
	sock, q := os.Getenv("SEMSEARCH_SOCKET"), os.Getenv("SEMSEARCH_QUERY")
	if sock == "" || q == "" {
		t.Skip("SEMSEARCH_SOCKET and SEMSEARCH_QUERY not set")
	}
	// Dial it relative to its directory: paths in temporary directories
	// can be longer than a socket address holds.
	t.Chdir(filepath.Dir(sock))
	sock = "./" + filepath.Base(sock)
	if out := os.Getenv("SEMSEARCH_SHOT"); out != "" && !filepath.IsAbs(out) {
		t.Fatal("SEMSEARCH_SHOT must be absolute")
	}
	home = func() string { h, _ := os.UserHomeDir(); return h }
	defer func() { home = func() string { return "/Users/me" } }()
	h := (&harness{t: t}).start(client.NewClient(sock))
	h.SetSize(760, 520)
	h.Type(q)
	h.Advance(debounce)
	h.flush()
	if h.a.statusErr != "" || h.a.searchErr != "" || len(h.a.hits) == 0 {
		t.Fatalf("status %q, search %q, %d hits", h.a.statusErr, h.a.searchErr, len(h.a.hits))
	}
	for _, s := range h.Texts() {
		t.Log(s)
	}
	// The event stream opens, and a pause shows without polling.
	posts := make(chan func(), 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.a.streamEvents(ctx, func(f func()) { posts <- f })
	next := func() {
		t.Helper()
		select {
		case f := <-posts:
			f()
		case <-time.After(10 * time.Second):
			t.Fatal("no event")
		}
		h.Advance(eventGap)
		h.Frame()
		h.flush()
	}
	next()
	if !h.a.live {
		t.Fatal("event stream not open")
	}
	h.a.pause(true)
	h.flush()
	next()
	if !strings.Contains(h.a.statusLine(), "aused") {
		t.Fatalf("pause not shown: %q", h.a.statusLine())
	}
	h.a.pause(false)
	h.flush()
	next()
	t.Log("live:", h.a.statusLine())
	if out := os.Getenv("SEMSEARCH_SHOT"); out != "" {
		h.shot(out + "-search.png")
		h.click("Settings")
		h.flush()
		if h.a.notice != "" {
			t.Fatal(h.a.notice)
		}
		if !h.a.pauseOnBattery {
			t.Error("pause on battery off; semd's default is on")
		}
		h.Advance(time.Second) // let the switch finish moving
		h.Frame()
		h.shot(out + "-settings.png")
	}
}

func (h *harness) shot(path string) {
	h.t.Helper()
	f, err := os.Create(path)
	if err != nil {
		h.t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, h.Image()); err != nil {
		h.t.Fatal(err)
	}
}

// While a status request is out, the window draws nothing more until
// its reply comes.
func TestIdleWhileStatusPending(t *testing.T) {
	h := newHarness(t)
	h.Advance(statusEvery)
	h.Frame()
	if len(h.queue) != 1 || !h.a.statusBusy {
		t.Fatalf("queue %d, busy %v", len(h.queue), h.a.statusBusy)
	}
	h.SetFocused(false) // no caret blinking
	h.Frame()
	h.builds = 0
	h.Advance(time.Minute) // semd is slow to answer
	if h.builds > 0 {
		t.Fatalf("%d frames while waiting for semd", h.builds)
	}
	h.flush()
	if h.a.statusBusy {
		t.Fatal("still busy")
	}
}

func TestFullDiskAccess(t *testing.T) {
	h := newHarness(t)
	h.a.helper = "/Applications/Kim Quy.app/Contents/Helpers/semd"
	h.semd.roots = client.RootsResponse{Roots: []string{"/Users/me/Documents", "/Users/me/Notes"}}
	h.semd.status = client.Status{Watch: "live", Roots: []client.RootStatus{
		{Path: "/Users/me/Documents", Error: "open /Users/me/Documents: operation not permitted"},
		{Path: "/Users/me/Notes"},
	}}
	h.Advance(statusEvery)
	h.flush()
	h.want("0 indexed · 1 folder unreadable")
	svc := &fakeService{info: agent.Info{Loaded: true, State: "running"}}
	h.a.svc = svc

	h.click("Settings")
	h.flush()
	h.click("Privacy")
	h.want("Full Disk Access")
	h.click("Open Privacy Settings")
	h.click("Show semd in Finder")
	h.click("Restart semd")
	h.flush()
	if h.shell.privacy != 1 || !slices.Equal(h.shell.revealed, []string{h.a.helper}) || svc.restarts != 1 || h.a.acting != "" {
		t.Fatalf("privacy %d revealed %v restarts %d", h.shell.privacy, h.shell.revealed, svc.restarts)
	}

	// Once granted, the guide goes away.
	h.semd.status.Roots[0].Error = ""
	h.Advance(statusEvery)
	h.flush()
	if slices.Contains(h.Texts(), "Open Privacy Settings") || slices.ContainsFunc(h.Texts(), func(s string) bool { return strings.Contains(s, "unreadable") }) {
		t.Fatalf("guide still shown: %q", h.Texts())
	}
}

func TestAgentError(t *testing.T) {
	h := newHarness(t)
	h.a.agentErr = "launchctl bootstrap: exit 5"
	h.Frame()
	h.want("semd could not be started: launchctl bootstrap: exit 5")
}

// TestLiveStatus: with the event stream open, events (not a 2 s poll)
// bring the status, coalesced; when it drops, polling comes back and the
// stream is reopened.
func TestLiveStatus(t *testing.T) {
	h := newHarness(t)
	type conn struct {
		evs  chan events.Event
		sync chan struct{} // a send returns once the events before it are handled
		done chan error
	}
	conns := make(chan conn)
	h.semd.stream = func(ctx context.Context, fn func(events.Event)) error {
		c := conn{make(chan events.Event), make(chan struct{}), make(chan error)}
		select {
		case conns <- c:
		case <-ctx.Done():
			return ctx.Err()
		}
		fn(events.Event{Type: "open"})
		for {
			select {
			case ev := <-c.evs:
				fn(ev)
			case <-c.sync:
			case err := <-c.done:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	posts := make(chan func(), 1000)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { h.a.streamEvents(ctx, func(f func()) { posts <- f }); close(stopped) }()
	defer func() { cancel(); <-stopped }()
	// apply runs n posts on the "main thread", then any work they start.
	apply := func(n int) {
		t.Helper()
		for range n {
			select {
			case f := <-posts:
				f()
			case <-time.After(5 * time.Second):
				t.Fatal("no post")
			}
		}
		h.Frame()
		h.flush()
	}
	c := <-conns
	apply(1) // open
	if !h.a.live {
		t.Fatal("not live")
	}
	h.Advance(eventGap)
	h.Frame()
	h.flush()
	n := h.semd.statuses
	h.Advance(5 * statusEvery) // no polling while live
	h.Frame()
	if len(h.queue) != 0 || h.semd.statuses != n {
		t.Fatalf("polled while live: %d queued, %d calls", len(h.queue), h.semd.statuses-n)
	}

	h.semd.status = client.Status{Indexed: 42}
	for range 500 {
		c.evs <- events.Event{Type: "indexed", Path: "/x"}
	}
	c.sync <- struct{}{}
	apply(1)
	if len(posts) != 0 {
		t.Fatalf("%d more posts for one burst", len(posts))
	}
	h.Advance(eventGap)
	h.Frame()
	h.flush()
	if h.semd.statuses != n+1 {
		t.Fatalf("%d status calls for a burst", h.semd.statuses-n)
	}
	h.want("42 indexed")
	if n := len(h.a.activity); n != maxActivity || h.a.activity[0].Type != "indexed" { // "connected" went first
		t.Fatalf("activity: %d, first %+v", n, h.a.activity[0])
	}

	// The stream drops: poll at once, then every statusEvery.
	c.done <- errors.New("connection reset")
	apply(1)
	if h.a.live {
		t.Fatal("still live")
	}
	h.Advance(eventGap)
	h.Frame()
	h.flush()
	h.Advance(statusEvery)
	h.Frame()
	h.flush()
	if h.semd.statuses != n+3 {
		t.Fatalf("%d status calls after the drop, want 2", h.semd.statuses-n-1)
	}
	select {
	case <-conns: // reconnected
	case <-time.After(3 * reconnectMin):
		t.Fatal("no reconnect")
	}
	apply(1)
	if !h.a.live {
		t.Fatal("not live again")
	}
}

// Open settings show new failures as the count changes.
func TestFailuresFollowStatus(t *testing.T) {
	h := newHarness(t)
	h.click("Settings")
	h.flush()
	h.click("Failed (0)")
	h.want("None.")
	h.semd.failures = []client.Failure{{Path: "/Users/me/bad.pdf", Error: "broken xref"}}
	h.semd.status.Failed = 1
	h.Advance(statusEvery)
	h.Frame()
	h.flush()
	h.want("Failed (1)")
}

// Escape clears the query, then hides the window.
func TestEscapeHides(t *testing.T) {
	h := newHarness(t)
	h.Type("soup")
	h.Frame()
	h.Key(0, ui.KeyEscape)
	h.Frame()
	if h.a.query != "" || h.shell.hidden != 0 {
		t.Fatalf("query %q, hidden %d", h.a.query, h.shell.hidden)
	}
	h.Key(0, ui.KeyEscape)
	h.Frame()
	if h.shell.hidden != 1 {
		t.Fatalf("hidden %d", h.shell.hidden)
	}
}

// The menu-bar menu follows the status: what is indexed, and whether the
// user paused indexing.
func TestMenuStatus(t *testing.T) {
	h := newHarness(t)
	changed := 0
	h.a.statusChanged = func() { changed++ }
	h.semd.status = client.Status{Indexed: 3, Paused: []string{"manual"}}
	h.Advance(statusEvery)
	h.Frame()
	h.flush()
	if changed != 1 || h.a.indexLine() != "3 indexed · paused (manual)" || h.a.pauseLabel() != "Resume Indexing" {
		t.Fatalf("changed %d, %q, %q", changed, h.a.indexLine(), h.a.pauseLabel())
	}
	h.a.togglePause()
	h.flush()
	if h.semd.resumed != 1 {
		t.Fatalf("resumed %d", h.semd.resumed)
	}
	h.semd.down = errors.New("connection refused")
	h.Advance(statusEvery)
	h.Frame()
	h.flush()
	if changed < 3 || h.a.indexLine() != "semd not connected" {
		t.Fatalf("changed %d, %q", changed, h.a.indexLine())
	}
	// Search results show in the footer only.
	h.semd.down = nil
	h.a.status, h.a.statusErr, h.a.hits = client.Status{Indexed: 3}, "", []client.Hit{pdfHit}
	if h.a.indexLine() != "3 indexed" || h.a.statusLine() != "3 indexed · 1 results in 0 ms" {
		t.Fatalf("%q / %q", h.a.indexLine(), h.a.statusLine())
	}
}

func TestTrayIcon(t *testing.T) {
	b, err := trayIcon()
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if r := img.Bounds(); r.Dx() != 32 || r.Dy() != 32 {
		t.Fatalf("size %v", r)
	}
	ink := 0
	for y := range 32 {
		for x := range 32 {
			r, g, b, a := img.At(x, y).RGBA()
			if r|g|b != 0 || a != 0 && a != 0xffff {
				t.Fatalf("(%d,%d) is not black or clear: %v", x, y, img.At(x, y))
			}
			if a != 0 {
				ink++
			}
		}
	}
	if ink < 100 || ink > 600 {
		t.Fatalf("%d inked pixels", ink)
	}
}

// Every settings tab fits the smallest window without scrolling, however
// much there is to show: long lists page. SEMSEARCH_SHOT=<prefix> saves a
// picture of each tab.
func TestSettingsFit(t *testing.T) {
	h := newHarness(t)
	const titleBar = 28 // MyGo's minimum is NSWindow's, which includes it
	h.SetSize(minWindowW, minWindowH-titleBar)
	h.a.helper = "/Applications/Kim Quy.app/Contents/Helpers/semd"
	h.a.svc = &fakeService{info: agent.Info{Loaded: true, State: "running", PID: 6888, Runs: 3, LastExit: "(never exited)",
		Program: "/Applications/Kim Quy.app/Contents/Helpers/semd"}}
	long := strings.Repeat("a very long reason that does not fit on a line ", 4)
	var roots []string
	var states []client.RootStatus
	for i := range 12 {
		r := fmt.Sprintf("/Users/me/Folder %02d", i)
		roots = append(roots, r)
		states = append(states, client.RootStatus{Path: r, Error: "open " + r + ": operation not permitted"})
	}
	var fails []client.Failure
	for i := range 23 {
		fails = append(fails, client.Failure{Path: fmt.Sprintf("/Users/me/Folder 01/broken %02d.pdf", i), Error: long})
	}
	h.semd.roots = client.RootsResponse{Roots: roots, Excludes: []string{"*.bak", "node_modules", "build/"}, DefaultExcludes: []string{".*", "*.tmp"}}
	h.semd.failures = fails
	var evs []events.Event
	for i := range 37 {
		evs = append(evs, events.Event{Time: time.Now(), Type: "failed", Path: fmt.Sprintf("/Users/me/Folder 01/broken %02d.pdf", i), Msg: long})
	}
	h.a.addActivity(evs)
	h.a.mdl = &fakeModels{dir: "/Users/me/Library/Application Support/Kim Quy/models", free: 1e9,
		have: map[string]bool{"text-q8": true}}
	h.a.model.installErr = long
	h.a.model.needRebuild = true
	h.semd.status = client.Status{
		Indexed: 123456, Pending: 789, Failed: len(fails), Scanning: true, Watch: "live",
		Paused:     []string{"manual", "on battery"},
		Embedder:   client.EmbedStatus{Model: "embeddinggemma-2/eb8a067456a3+v9b18ee382729+aa163c6dc2e47", Precision: "affine8-g64", Dims: 256},
		Store:      &client.StoreStats{Files: 123456, Records: 987654, Segments: 12},
		StoreError: long,
		Roots:      states,
	}
	h.Advance(statusEvery)
	h.flush()
	h.click("Settings")
	h.flush()
	h.a.notice = "Cannot change folders: " + long // the worst case: a notice too
	h.Frame()

	// The modal is centered, 20 in from the window at most, and pads the
	// sheet by 20; it clips what overflows.
	const margin, pad = 20, 20
	winW, winH := float32(minWindowW), float32(minWindowH-titleBar)
	if sheetW+2*pad > winW-2*margin || sheetH+2*pad > winH-2*margin {
		t.Fatalf("a %dx%d sheet does not fit a %.0fx%.0f window", sheetW, sheetH, winW, winH)
	}
	left, top := (winW-sheetW)/2, (winH-sheetH)/2
	right, bottom := left+sheetW, top+sheetH
	if r, ok := h.Find("Done"); !ok || r.Y < top-0.5 || r.X+r.W > right+0.5 {
		t.Fatalf("Done at %+v, outside the sheet (%.0f,%.0f)–(%.0f,%.0f)", r, left, top, right, bottom)
	}
	if r, ok := h.Find("Service"); !ok || r.X+r.W > right+0.5 {
		t.Errorf("the tabs end at %.0f, past the sheet (%.0f)", r.X+r.W, right)
	}
	for _, tab := range []struct{ name, last string }{
		{"Folders", "Next folders"},
		{"Excluded", "Save Exclusions"},
		{"Indexing", "Rebuild Index…"},
		{"Model", "Use Files I Have…"},
		{"Privacy", "Restart semd"},
		{"Failed (23)", "Next failures"},
		{"Activity", "Next activity"},
		{"Service", "Uninstall Kim Quy…"},
	} {
		h.click(tab.name)
		h.flush()
		r, ok := h.Find(tab.last)
		if !ok {
			t.Errorf("%s: no %q in %q", tab.name, tab.last, h.Texts())
			continue
		}
		// Find gives the visible part: a clipped element is short.
		if r.Y+r.H > bottom+0.5 || r.X+r.W > right+0.5 || r.H < 15 {
			t.Errorf("%s: %q at %+v, not all in the sheet (%.0f,%.0f)–(%.0f,%.0f)", tab.name, tab.last, r, left, top, right, bottom)
		}
		if out := os.Getenv("SEMSEARCH_SHOT"); out != "" {
			h.shot(fmt.Sprintf("%s-%s.png", out, strings.Fields(tab.name)[0]))
		}
	}

	// Paging: the last page shows the rest and Next stops there.
	h.click("Folders")
	h.want("1–5 of 12")
	h.click("Next folders")
	h.click("Next folders")
	h.want("11–12 of 12")
	h.want("~/Folder 11")
	if h.HasText("~/Folder 00") {
		t.Error("first page still shown")
	}
	h.click("Next folders") // disabled: stays
	h.want("11–12 of 12")
	h.click("Failed (23)")
	h.want("1–6 of 23")

	// A list that shrinks under its page moves to its last page.
	h.a.folderPage = 2
	h.a.roots.Roots = roots[:3]
	h.click("Folders")
	if h.a.folderPage != 0 || h.HasText("of 3") {
		t.Fatalf("page %d, %q", h.a.folderPage, h.Texts())
	}
	h.want("~/Folder 02")
}

func TestPage(t *testing.T) {
	for _, c := range []struct{ p, n, per, wantP, lo, hi int }{
		{0, 0, 5, 0, 0, 0},
		{3, 0, 5, 0, 0, 0},
		{0, 5, 5, 0, 0, 5},
		{1, 5, 5, 0, 0, 5},
		{1, 6, 5, 1, 5, 6},
		{-1, 6, 5, 0, 0, 5},
		{9, 12, 5, 2, 10, 12},
	} {
		p := c.p
		lo, hi := page(&p, c.n, c.per)
		if p != c.wantP || lo != c.lo || hi != c.hi {
			t.Errorf("page(%d, %d, %d) = %d, %d, %d; want %d, %d, %d", c.p, c.n, c.per, p, lo, hi, c.wantP, c.lo, c.hi)
		}
	}
	defer func() {
		if recover() == nil {
			t.Error("per 0 did not panic")
		}
	}()
	p := 0
	page(&p, 1, 0)
}

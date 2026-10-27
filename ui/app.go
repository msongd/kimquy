package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/msongd/kimquy/internal/agent"
	"github.com/msongd/kimquy/internal/client"
	"github.com/msongd/kimquy/internal/events"
	"github.com/msongd/kimquy/internal/kind"
)

const (
	debounce       = 200 * time.Millisecond // typing pause before a search
	statusEvery    = 2 * time.Second        // status poll without the event stream
	liveEvery      = 30 * time.Second       // with it: a safety net only
	eventGap       = 250 * time.Millisecond // between status requests events cause
	reconnectMin   = time.Second            // to the event stream, doubling
	reconnectMax   = 30 * time.Second
	requestTimeout = 30 * time.Second // a search may wait for the model to load

	// The settings sheet has a fixed size and never scrolls: the window
	// cannot get smaller than the sheet needs, and long lists page. A
	// Modal is at most the window less 20 around it, and pads its content
	// by 20.
	minWindowW   = 680
	minWindowH   = 508 // the frame: the content is a title bar (28) less
	sheetW       = minWindowW - 2*(20+20)
	sheetH       = minWindowH - 28 - 2*(20+20)
	foldersPage  = 5 // a folder takes up to two lines (with its error)
	failuresPage = 6
)

// The tabs of the settings sheet.
const (
	tabFolders = iota
	tabExclusions
	tabIndexing
	tabModel
	tabPrivacy
	tabFailures
	tabActivity
	tabService
)

// backend is what the UI asks of semd; *client.Client implements it.
type backend interface {
	Search(context.Context, client.SearchRequest) (client.SearchResponse, error)
	Status(context.Context) (client.Status, error)
	Roots(context.Context) (client.RootsResponse, error)
	SetRoots(context.Context, client.RootsRequest) (client.RootsResponse, error)
	Failures(context.Context) ([]client.Failure, error)
	Settings(context.Context) (client.Settings, error)
	SetSettings(context.Context, client.Settings) (client.Settings, error)
	Pause(context.Context) error
	Resume(context.Context) error
	Reindex(context.Context) error
	// Events streams semd's events until ctx ends or the connection drops;
	// the first is "open".
	Events(context.Context, func(events.Event)) error
}

// shell is what the UI asks of the desktop.
type shell interface {
	Open(path string) error
	Reveal(path string) error
	// PickFolders asks the user for folders; none if canceled. It blocks,
	// so it is called off the main thread.
	PickFolders() ([]string, error)
	// PickModel asks for a folder or app holding the model; "" if
	// canceled. It blocks.
	PickModel() (string, error)
	// OpenPrivacySettings opens System Settings at Full Disk Access.
	OpenPrivacySettings() error
	// HideWindow hides the window; the menu-bar icon and the global
	// shortcut bring it back.
	HideWindow()
	// Quit quits the app.
	Quit()
}

// app is the state the window shows. It is read and changed on the main
// thread only: work off it goes through async, whose result is applied
// back on the main thread.
type app struct {
	be backend
	sh shell
	// helper is the semd executable the user grants Full Disk Access to;
	// empty when not running from the app bundle.
	helper string
	// agentErr is why semd could not be installed as a LaunchAgent.
	agentErr string
	// svc controls semd's LaunchAgent; nil when the UI does not manage it.
	svc service
	// mdl installs semd's model; nil when the UI does not manage it.
	mdl   models
	model modelState
	// statusChanged, if set, is called on the main thread whenever the
	// status or its error changes: the menu-bar menu shows it.
	statusChanged func()
	// async runs work off the main thread, then the func it returns (if
	// not nil) on the main thread, redrawing after.
	async func(work func() func())

	query     string
	kinds     []string // the kinds to search, by name; none for all
	root      string   // the folder to search; "" for all
	limit     int      // results at most
	input     ui.Handle
	due       time.Time // when to search for query; zero when none is due
	seq       int       // of the latest search; older replies are dropped
	searching bool
	hits      []client.Hit
	took      int64
	searchErr string
	chosen    int
	list      ui.ListState
	notice    string // the last action that failed, shown until the next

	status     client.Status
	statusErr  string
	statusDue  time.Time
	statusBusy bool
	statusAt   time.Time // when the last status request went out
	live       bool      // the event stream is open
	stale      bool      // an event came after the last status request

	settingsOpen   bool
	settingsTab    int
	folderPage     int
	failurePage    int
	roots          client.RootsResponse
	excludes       string // one pattern per line, as edited
	pauseOnBattery bool
	failures       []client.Failure
	changing       bool // a change of roots or settings is in flight
	askReindex     bool

	activity       []events.Event // oldest first, at most maxActivity
	activityFrozen []events.Event // the log as held off its first page
	activityPage   int

	info       agent.Info // what launchd says of semd, on the Service tab
	infoErr    string
	infoLoaded bool
	infoBusy   bool
	infoDue    time.Time
	acting     string // the action on the agent in flight, e.g. "Stopping semd…"

	askUninstall bool
	uninstalling bool
	uninstalled  []string // what was removed; not nil once done
	uninstallErr string
}

func newApp(be backend, sh shell, async func(func() func())) *app {
	if be == nil || sh == nil || async == nil {
		panic("newApp: nil dependency")
	}
	return &app{be: be, sh: sh, async: async, limit: defaultLimit}
}

// call runs fn against semd off the main thread with a timeout, then done
// with its error on the main thread.
func (a *app) call(fn func(context.Context) error, done func(error)) {
	a.async(func() func() {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		err := fn(ctx)
		return func() { done(err) }
	})
}

func (a *app) view(c *ui.Context) {
	t := c.Theme()
	c.Root().Background(t.Background)
	now := c.Now()
	a.tick(c, now)

	if a.needsModel() {
		a.setupView(c)
		if a.svc != nil {
			a.uninstallView(c)
		}
		return
	}
	if !a.settingsOpen && !a.modalOpen() {
		if c.Shortcut(0, ui.KeyEscape) {
			if a.query != "" {
				a.query = ""
				a.queryChanged(now)
			} else {
				a.sh.HideWindow()
			}
		}
		if c.Shortcut(0, ui.KeyDown) {
			a.choose(a.chosen + 1)
		}
		if c.Shortcut(0, ui.KeyUp) {
			a.choose(a.chosen - 1)
		}
		if c.Shortcut(0, ui.KeyEnter) {
			a.open(false)
		}
		if c.Shortcut(ui.Cmd, ui.KeyEnter) {
			a.open(true)
		}
	}
	if c.Shortcut(ui.Cmd, ui.KeyComma) && !a.modalOpen() {
		a.openSettings()
	}

	ui.Column(c).Fill().Children(func() {
		ui.Row(c).Padding(14, 16).Gap(10).AlignItems(ui.Center).Children(func() {
			in := ui.TextInputBase(c, &a.query).Bind(&a.input).AutoFocus().Grow(1).FontSize(20).
				Placeholder("Search your files").Label("Search")
			if in.Changed() {
				a.queryChanged(now)
			}
			if in.Submitted() { // the input takes Enter from Shortcut
				a.open(false)
			}
			if a.searching {
				ui.Spinner(c)
			}
			if ui.Button(c, "Settings").Tooltip("⌘,").Clicked() {
				a.openSettings()
			}
		})
		a.filtersRow(c)
		ui.Divider(c)
		if msg := a.problem(); msg != "" {
			ui.Text(c, msg).TextColor(t.Danger).Padding(8, 16).Wrap()
			ui.Divider(c)
		}
		a.list.Selected = &a.chosen
		a.list.Key = func(i int) any { return hitKey(a.hits[i]) }
		a.list.Label = func(i int) string { return filepath.Base(a.hits[i].Path) }
		list := ui.List(c, &a.list, len(a.hits), func(i int) { a.row(c, i) }).Grow(1).Padding(6).Children(func() {
			if len(a.hits) == 0 {
				ui.Text(c, a.emptyText()).TextColor(t.TextMuted).Padding(16)
			}
		})
		if list.Submitted() {
			a.open(false)
		}
		ui.Divider(c)
		ui.Row(c).Padding(8, 16).Gap(16).Children(func() {
			ui.Text(c, a.statusLine()).FontSize(11).TextColor(t.TextMuted).Grow(1).SingleLine()
			ui.Text(c, "↑↓ Choose   ↵ Open   ⌘↵ Show in Finder").FontSize(11).TextColor(t.TextMuted)
		})
	})
	if a.settingsOpen {
		ui.Modal(c, &a.settingsOpen, func() { a.settings(c) })
	}
	switch ui.AlertDialog(c, &a.askReindex, "Rebuild the index?",
		"Every file is read and embedded again, which can take a long time.",
		"Cancel", "Rebuild") {
	case 1:
		a.reindex()
	}
	a.addPartView(c)
	if a.svc != nil {
		a.uninstallView(c)
	}
}

// modalOpen reports whether a dialog of the uninstall flow, or one adding
// to the model, shows, which takes the keys.
func (a *app) modalOpen() bool {
	return a.askUninstall || a.uninstalling || a.uninstalled != nil || a.uninstallErr != "" ||
		a.model.askAdd.Dir != ""
}

// tick starts what is due: the debounced search and the status poll.
func (a *app) tick(c *ui.Context, now time.Time) {
	if a.mdl != nil && a.model.have == nil {
		a.loadModel()
	}
	if !a.due.IsZero() {
		if now.Before(a.due) {
			c.After(a.due.Sub(now))
		} else {
			a.due = time.Time{}
			a.search(strings.TrimSpace(a.query))
		}
	}
	if next := a.pollStatus(now); !next.IsZero() {
		c.After(next.Sub(now))
	}
	if next := a.pollInfo(now); !next.IsZero() {
		c.After(next.Sub(now))
	}
}

// pollStatus asks semd for the status if it is due, and returns when it
// is next due: zero while a request is out, whose reply comes back to the
// main thread. The view calls it at each frame; while the window is hidden
// and draws no frames, main calls it every statusEvery.
func (a *app) pollStatus(now time.Time) time.Time {
	if a.statusBusy {
		return time.Time{}
	}
	due := a.statusDue
	if soon := a.statusAt.Add(eventGap); a.stale && soon.Before(due) {
		due = soon
	}
	if now.Before(due) {
		return due
	}
	every := statusEvery
	if a.live {
		every = liveEvery
	}
	a.stale, a.statusAt, a.statusDue = false, now, now.Add(every)
	a.refreshStatus()
	return time.Time{}
}

// streamEvents keeps semd's event stream open until ctx ends, reconnecting
// with backoff. An event goes to the activity log and marks the status
// stale: the view fetches it at most every eventGap, so a burst of
// thousands of "indexed" costs a few requests and redraws. Without the
// stream the view polls every statusEvery. post runs a func on the main
// thread and redraws.
func (a *app) streamEvents(ctx context.Context, post func(func())) {
	var (
		queued atomic.Bool // a stale mark is posted and not yet applied
		q      activityQueue
	)
	note := func(ev events.Event) {
		q.add(ev)
		if !queued.Swap(true) {
			post(func() { queued.Store(false); a.stale = true; a.addActivity(q.take()) })
		}
	}
	wait, last := reconnectMin, ""
	for ctx.Err() == nil {
		opened := false
		err := a.be.Events(ctx, func(ev events.Event) {
			if ev.Type == "open" {
				opened = true
				q.add(events.Event{Time: time.Now(), Type: "connected", Msg: "semd's events show as they come"})
				post(func() { a.live, a.stale = true, true; a.addActivity(q.take()) }) // events before it were missed
				return
			}
			note(ev)
		})
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("semd closed the stream")
		}
		if opened {
			q.add(events.Event{Time: time.Now(), Type: "disconnected", Msg: err.Error()})
		}
		post(func() { a.live, a.stale = false, true; a.addActivity(q.take()) }) // the poll shows semd down
		if opened || err.Error() != last {                                      // not every retry
			log.Printf("semd event stream: %v; polling every %v", err, statusEvery)
		}
		last = err.Error()
		if opened {
			wait = reconnectMin
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, reconnectMax)
	}
}

func (a *app) queryChanged(now time.Time) {
	if strings.TrimSpace(a.query) == "" {
		a.seq++ // drop a search in flight
		a.due, a.hits, a.searching, a.searchErr = time.Time{}, nil, false, ""
		return
	}
	a.due = now.Add(debounce)
}

func (a *app) search(q string) {
	if q == "" {
		return
	}
	a.seq++
	seq := a.seq
	req := a.searchRequest(q)
	a.searching = true
	a.async(func() func() {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		res, err := a.be.Search(ctx, req)
		return func() {
			if seq != a.seq {
				return // superseded
			}
			a.searching = false
			if err != nil {
				a.searchErr = "Search failed: " + err.Error()
				return
			}
			a.searchErr, a.hits, a.took = "", res.Hits, res.TookMS
			a.choose(0)
		}
	})
}

func (a *app) choose(i int) {
	a.chosen = max(0, min(i, len(a.hits)-1))
	if len(a.hits) > 0 {
		a.list.ScrollIntoView(a.chosen)
	}
}

// open opens the chosen hit, or shows it in Finder.
func (a *app) open(reveal bool) {
	if a.chosen < 0 || a.chosen >= len(a.hits) {
		return
	}
	p := a.hits[a.chosen].Path
	verb, fn := "open", a.sh.Open
	if reveal {
		verb, fn = "show", a.sh.Reveal
	}
	a.notice = ""
	if err := fn(p); err != nil {
		a.notice = fmt.Sprintf("Cannot %s %s: %v", verb, p, err)
	}
}

func (a *app) refreshStatus() {
	a.statusBusy = true
	var s client.Status
	a.call(func(ctx context.Context) (err error) {
		s, err = a.be.Status(ctx)
		return err
	}, func(err error) {
		a.statusBusy = false
		if a.statusChanged != nil {
			defer a.statusChanged()
		}
		if err != nil {
			a.statusErr = err.Error()
			return
		}
		failed := a.status.Failed
		a.status, a.statusErr = s, ""
		a.rootsUpdated()
		if a.settingsOpen && s.Failed != failed {
			a.loadFailures()
		}
	})
}

// loadFailures refreshes the list in settings.
func (a *app) loadFailures() {
	var fails []client.Failure
	a.call(func(ctx context.Context) (err error) {
		fails, err = a.be.Failures(ctx)
		return err
	}, func(err error) {
		if err != nil {
			a.notice = "Cannot load failures: " + err.Error()
			return
		}
		a.failures = fails
	})
}

// problem is the error banner: semd unreachable, or a failed action.
func (a *app) problem() string {
	switch {
	case a.agentErr != "":
		return "semd could not be started: " + a.agentErr
	case a.statusErr != "":
		return "semd is not reachable (is it running?): " + a.statusErr
	case a.searchErr != "":
		return a.searchErr
	}
	return a.notice
}

func (a *app) emptyText() string {
	switch {
	case strings.TrimSpace(a.query) == "":
		return "Type to search the contents of your files."
	case a.searching || !a.due.IsZero():
		return "Searching…"
	case a.searchErr != "":
		return ""
	case a.filtered():
		return "No matches in the kinds and folder chosen."
	}
	return "No matches."
}

func (a *app) statusLine() string {
	line := a.indexLine()
	if len(a.hits) > 0 && a.statusErr == "" {
		line += fmt.Sprintf(" · %d results in %d ms", len(a.hits), a.took)
	}
	return line
}

// indexLine is the state of the index, for the footer and the menu bar.
func (a *app) indexLine() string {
	if a.statusErr != "" {
		return "semd not connected"
	}
	s := a.status
	parts := []string{fmt.Sprintf("%d indexed", s.Indexed)}
	if s.Pending > 0 {
		parts = append(parts, fmt.Sprintf("%d pending", s.Pending))
	}
	if s.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", s.Failed))
	}
	if s.Scanning {
		parts = append(parts, "scanning")
	}
	switch n := a.unreadable(); n {
	case 0:
	case 1:
		parts = append(parts, "1 folder unreadable")
	default:
		parts = append(parts, fmt.Sprintf("%d folders unreadable", n))
	}
	if len(s.Paused) > 0 {
		parts = append(parts, "paused ("+strings.Join(s.Paused, ", ")+")")
	}
	return strings.Join(parts, " · ")
}

// row shows a hit: its kind, name, folder and place in the file, the
// matching text, and its score.
func (a *app) row(c *ui.Context, i int) {
	h := a.hits[i]
	t := c.Theme()
	bg, text, muted := ui.Transparent, t.Text, t.TextMuted
	if i == a.chosen {
		bg, text, muted = t.Accent.Alpha(0.85), t.AccentText, t.AccentText.Alpha(0.75)
	}
	ui.Column(c).Padding(6, 10).Gap(2).Radius(8).Background(bg).Children(func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Badge(c, kindLabel(h.Kind))
			name := filepath.Base(h.Path)
			if h.Stale {
				name += "  (changed since indexed)"
			}
			ui.Text(c, name).TextColor(text).Bold().SingleLine().Grow(1)
			ui.Text(c, fmt.Sprintf("%.2f", h.Score)).FontSize(11).TextColor(muted)
		})
		where := tildePath(filepath.Dir(h.Path))
		if loc := locatorText(h.Locator); loc != "" {
			where += " · " + loc
		}
		ui.Text(c, where).FontSize(11).TextColor(muted).SingleLine()
		if h.Snippet != "" {
			ui.Text(c, oneLine(h.Snippet)).FontSize(12).TextColor(muted).SingleLine()
		}
	})
}

func hitKey(h client.Hit) string { return h.Path + "#" + h.Locator.String() }

func kindLabel(k string) string {
	switch k {
	case "pdf":
		return "PDF"
	case "":
		return "?"
	}
	return strings.ToUpper(k[:1]) + k[1:]
}

// locatorText says where in the file a hit is: a page or a time span.
func locatorText(l kind.Locator) string {
	switch l.Kind {
	case kind.LocPage:
		return fmt.Sprintf("page %d", l.Page)
	case kind.LocTime:
		return clock(l.Start) + "–" + clock(l.End)
	}
	return ""
}

// clock formats milliseconds as m:ss, or h:mm:ss from an hour.
func clock(ms int64) string {
	s := max(ms, 0) / 1000
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

var home = func() string { // var for tests
	h, _ := os.UserHomeDir()
	return h
}

func tildePath(p string) string {
	if h := home(); h != "" {
		if rest, ok := strings.CutPrefix(p, h); ok && (rest == "" || rest[0] == '/') {
			return "~" + rest
		}
	}
	return p
}

// openSettings shows the settings sheet and loads what it shows.
func (a *app) openSettings() {
	a.settingsOpen = true
	a.notice = ""
	a.loadModel()
	var (
		roots client.RootsResponse
		set   client.Settings
		fails []client.Failure
	)
	a.call(func(ctx context.Context) (err error) {
		if roots, err = a.be.Roots(ctx); err != nil {
			return err
		}
		if set, err = a.be.Settings(ctx); err != nil {
			return err
		}
		fails, err = a.be.Failures(ctx)
		return err
	}, func(err error) {
		if err != nil {
			a.notice = "Cannot load settings: " + err.Error()
			return
		}
		a.setRoots(roots)
		a.pauseOnBattery = set.PauseOnBattery
		a.failures = fails
	})
}

func (a *app) setRoots(r client.RootsResponse) {
	a.roots = r
	a.excludes = strings.Join(r.Excludes, "\n")
}

func (a *app) settings(c *ui.Context) {
	t := c.Theme()
	ui.Column(c).Width(sheetW).Height(sheetH).Gap(14).Children(func() {
		ui.Row(c).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "Settings").FontSize(17).Bold().Grow(1)
			if ui.Button(c, "Done").Clicked() {
				a.settingsOpen = false
			}
		})
		ui.Tabs(c, &a.settingsTab, "Folders", "Excluded", "Indexing", "Model", "Privacy",
			fmt.Sprintf("Failed (%d)", len(a.failures)), "Activity", "Service")
		if a.notice != "" {
			ui.Text(c, a.notice).TextColor(t.Danger).SingleLine().Tooltip(a.notice)
		}
		ui.Column(c).Grow(1).Gap(10).Children(func() {
			switch a.settingsTab {
			case tabFolders:
				a.rootsSection(c)
			case tabExclusions:
				a.excludesSection(c)
			case tabIndexing:
				a.indexingSection(c)
			case tabModel:
				a.modelSection(c)
			case tabPrivacy:
				a.accessSection(c)
			case tabFailures:
				a.failuresSection(c)
			case tabActivity:
				a.activitySection(c)
			case tabService:
				a.serviceSection(c)
			default:
				panic(fmt.Sprintf("settings: no tab %d", a.settingsTab))
			}
		})
	})
}

// page clamps *p to the pages of n items, per on each, and returns the
// bounds of its items. The list may have shrunk since *p was set.
func page(p *int, n, per int) (lo, hi int) {
	if per <= 0 {
		panic("page: per must be positive")
	}
	last := max(0, (n-1)/per)
	*p = min(max(*p, 0), last)
	lo = *p * per
	return lo, min(lo+per, n)
}

// pager shows the position in a list of n items and buttons to move
// through it; nothing when it fits on one page. name tells the lists apart
// for assistive technology.
func pager(c *ui.Context, p *int, n, per int, name string) {
	if n <= per {
		return
	}
	lo, hi := page(p, n, per)
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Text(c, fmt.Sprintf("%d–%d of %d", lo+1, hi, n)).FontSize(11).TextColor(c.Theme().TextMuted)
		if ui.Button(c, "‹").Label("Previous " + name).Tooltip("Previous").Disabled(lo == 0).Clicked() {
			*p--
		}
		if ui.Button(c, "›").Label("Next " + name).Tooltip("Next").Disabled(hi == n).Clicked() {
			*p++
		}
	})
}

func (a *app) rootsSection(c *ui.Context) {
	t := c.Theme()
	if len(a.roots.Roots) == 0 {
		ui.Text(c, "No folders yet: add the ones to search.").TextColor(t.TextMuted)
	}
	lo, hi := page(&a.folderPage, len(a.roots.Roots), foldersPage)
	for _, r := range a.roots.Roots[lo:hi] {
		ui.Row(c).Key(r).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Column(c).Grow(1).Children(func() {
				ui.Text(c, tildePath(r)).SingleLine()
				if e := a.rootError(r); e != "" {
					ui.Text(c, e).FontSize(11).TextColor(t.Danger).SingleLine().Tooltip(e)
				}
			})
			if ui.Button(c, "Remove").Label("Remove " + r).Disabled(a.changing).Clicked() {
				a.changeRoots(slices.DeleteFunc(slices.Clone(a.roots.Roots), func(s string) bool { return s == r }), nil)
			}
		})
	}
	ui.Column(c).Grow(1)
	ui.Row(c).AlignItems(ui.Center).Children(func() {
		if ui.Button(c, "Add Folders…").Disabled(a.changing).Clicked() {
			a.addFolders()
		}
		ui.Column(c).Grow(1)
		pager(c, &a.folderPage, len(a.roots.Roots), foldersPage, "folders")
	})
}

func (a *app) rootError(root string) string {
	for _, r := range a.status.Roots {
		if r.Path == root && r.Error != "" {
			if denied(r.Error) {
				return r.Error + " — semd needs Full Disk Access (see Privacy)."
			}
			return r.Error
		}
	}
	return ""
}

// denied reports whether a root error is macOS privacy protection (TCC).
func denied(err string) bool { return strings.Contains(err, "operation not permitted") }

// unreadable counts the roots semd could not read.
func (a *app) unreadable() int {
	n := 0
	for _, r := range a.status.Roots {
		if r.Error != "" {
			n++
		}
	}
	return n
}

// accessSection guides the user through granting Full Disk Access when
// macOS keeps semd out of a folder.
func (a *app) accessSection(c *ui.Context) {
	t := c.Theme()
	if !slices.ContainsFunc(a.status.Roots, func(r client.RootStatus) bool { return denied(r.Error) }) {
		ui.Text(c, "macOS lets semd read all your folders.").TextColor(t.TextMuted).Wrap()
		return
	}
	ui.Text(c, "Full Disk Access").Bold()
	msg := "macOS keeps semd out of some folders. In System Settings › Privacy & Security › Full Disk Access, click +, add semd, and turn it on. semd looks at the folders again every minute; restart it if they stay unreadable."
	if a.helper != "" {
		msg = "macOS keeps semd out of some folders. In System Settings › Privacy & Security › Full Disk Access, click +, add semd (Show semd in Finder, then drag it into the list), and turn it on. semd looks at the folders again every minute; restart it if they stay unreadable."
	}
	ui.Text(c, msg).TextColor(t.TextMuted).Wrap()
	ui.Row(c).Gap(8).Children(func() {
		if ui.Button(c, "Open Privacy Settings").Clicked() {
			if err := a.sh.OpenPrivacySettings(); err != nil {
				a.notice = "Could not open System Settings: " + err.Error()
			}
		}
		if a.helper != "" && ui.Button(c, "Show semd in Finder").Clicked() {
			if err := a.sh.Reveal(a.helper); err != nil {
				a.notice = "Could not show semd: " + err.Error()
			}
		}
		if a.svc != nil && ui.Button(c, "Restart semd").Disabled(a.acting != "").Clicked() {
			a.serviceAction("Restarting semd…", a.svc.Restart)
		}
	})
}

func (a *app) excludesSection(c *ui.Context) {
	t := c.Theme()
	ui.Text(c, "One pattern per line. With a “/” a pattern matches the path inside a folder, otherwise a file or folder name. Always excluded: "+
		strings.Join(a.roots.DefaultExcludes, " ")).FontSize(11).TextColor(t.TextMuted).Wrap()
	ui.TextArea(c, &a.excludes).Label("Excluded patterns").Grow(1)
	edited := patterns(a.excludes)
	ui.Row(c).Children(func() {
		if ui.Button(c, "Save Exclusions").Disabled(a.changing || slices.Equal(edited, a.roots.Excludes)).Clicked() {
			a.changeRoots(a.roots.Roots, edited)
		}
	})
}

// patterns splits the edited excludes into trimmed, non-empty lines; never
// nil, as nil would keep semd's excludes rather than clear them.
func patterns(s string) []string {
	out := []string{}
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (a *app) indexingSection(c *ui.Context) {
	t := c.Theme()
	s := a.status
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Text(c, "Pause indexing on battery").Grow(1)
		if ui.Switch(c, &a.pauseOnBattery).Label("Pause indexing on battery").Disabled(a.changing).Changed() {
			a.setPauseOnBattery(a.pauseOnBattery)
		}
	})
	ui.Text(c, a.statusLine()).TextColor(t.TextMuted).Wrap()
	details := []string{"Watching: " + orDash(s.Watch)}
	if s.Embedder.Model != "" {
		details = append(details, fmt.Sprintf("Model: %s (%s, %d dims)", s.Embedder.Model, s.Embedder.Precision, s.Embedder.Dims))
	}
	if s.Store != nil {
		details = append(details, fmt.Sprintf("Store: %d files, %d vectors in %d segments", s.Store.Files, s.Store.Records, s.Store.Segments))
	}
	if s.StoreError != "" {
		details = append(details, "Store error: "+s.StoreError)
	}
	for _, d := range details { // one line each, so the tab stays its size
		ui.Text(c, d).FontSize(11).TextColor(t.TextMuted).SingleLine().Tooltip(d)
	}
	ui.Row(c).Gap(8).Children(func() {
		if ui.Button(c, a.pauseLabel()).Clicked() {
			a.togglePause()
		}
		if ui.Button(c, "Rebuild Index…").Clicked() {
			a.askReindex = true
		}
	})
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func (a *app) failuresSection(c *ui.Context) {
	t := c.Theme()
	if len(a.failures) == 0 {
		ui.Text(c, "None.").TextColor(t.TextMuted)
	}
	lo, hi := page(&a.failurePage, len(a.failures), failuresPage)
	for _, f := range a.failures[lo:hi] {
		ui.Column(c).Key(f.Path).Children(func() {
			ui.Text(c, tildePath(f.Path)).FontSize(12).SingleLine().Tooltip(f.Path)
			ui.Text(c, f.Error).FontSize(11).TextColor(t.TextMuted).SingleLine().Tooltip(f.Error)
		})
	}
	ui.Column(c).Grow(1)
	ui.Row(c).AlignItems(ui.Center).Children(func() {
		ui.Column(c).Grow(1)
		pager(c, &a.failurePage, len(a.failures), failuresPage, "failures")
	})
}

// addFolders asks for folders and adds them to the roots.
func (a *app) addFolders() {
	a.changing = true
	a.async(func() func() {
		dirs, err := a.sh.PickFolders()
		return func() {
			a.changing = false
			if err != nil {
				a.notice = "Cannot choose folders: " + err.Error()
				return
			}
			roots := slices.Clone(a.roots.Roots)
			for _, d := range dirs {
				if !slices.Contains(roots, d) {
					roots = append(roots, d)
				}
			}
			if len(roots) != len(a.roots.Roots) {
				a.changeRoots(roots, nil)
			}
		}
	})
}

// changeRoots sets the roots, and the excludes unless nil (kept as they are,
// with any unsaved edits left in the editor).
func (a *app) changeRoots(roots, excludes []string) {
	if roots == nil {
		roots = []string{} // semd requires the field: [] removes all
	}
	a.changing, a.notice = true, ""
	var res client.RootsResponse
	a.call(func(ctx context.Context) (err error) {
		res, err = a.be.SetRoots(ctx, client.RootsRequest{Roots: roots, Excludes: excludes})
		return err
	}, func(err error) {
		a.changing = false
		if err != nil {
			a.notice = "Cannot change folders: " + err.Error()
			return
		}
		edited := a.excludes
		a.setRoots(res)
		if excludes == nil {
			a.excludes = edited
		}
		a.statusDue = time.Time{} // show the new roots' state soon
	})
}

func (a *app) setPauseOnBattery(on bool) {
	a.changing, a.notice = true, ""
	var got client.Settings
	a.call(func(ctx context.Context) (err error) {
		got, err = a.be.SetSettings(ctx, client.Settings{PauseOnBattery: on})
		return err
	}, func(err error) {
		a.changing = false
		if err != nil {
			a.pauseOnBattery = !on
			a.notice = "Cannot change the setting: " + err.Error()
			return
		}
		a.pauseOnBattery = got.PauseOnBattery
	})
}

// pauseLabel names what togglePause does.
func (a *app) pauseLabel() string {
	if a.pausedByUser() {
		return "Resume Indexing"
	}
	return "Pause Indexing"
}

func (a *app) pausedByUser() bool { return slices.Contains(a.status.Paused, "manual") }

func (a *app) togglePause() { a.pause(!a.pausedByUser()) }

func (a *app) pause(on bool) {
	fn := a.be.Resume
	if on {
		fn = a.be.Pause
	}
	a.call(fn, func(err error) {
		if err != nil {
			a.notice = "Cannot pause or resume: " + err.Error()
		}
		a.statusDue = time.Time{}
	})
}

func (a *app) reindex() {
	a.call(a.be.Reindex, func(err error) {
		if err != nil {
			a.notice = "Cannot rebuild the index: " + err.Error()
		} else {
			a.model.needRebuild = false
		}
		a.statusDue = time.Time{}
	})
}

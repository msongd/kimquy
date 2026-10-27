package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/msongd/kimquy/internal/agent"
	"github.com/msongd/kimquy/internal/client"
	"github.com/msongd/kimquy/internal/events"
	"github.com/msongd/kimquy/internal/fsutil"
)

// chooseFrom opens the menu of a menu button and chooses an item.
func (h *harness) chooseFrom(button, item string) {
	h.t.Helper()
	h.click(button)
	if err := h.ChooseMenuItem(item); err != nil {
		h.t.Fatalf("%s › %s: %v (menu %q)", button, item, err, h.Menu())
	}
	h.flush()
}

func TestSearchOptions(t *testing.T) {
	h := newHarness(t)
	h.semd.status = client.Status{Roots: []client.RootStatus{{Path: "/Users/me/Notes"}, {Path: "/Users/me/Work"}}}
	h.Advance(statusEvery)
	h.flush()
	h.Type("budget")
	h.Advance(debounce)
	h.flush()
	last := func() client.SearchRequest {
		t.Helper()
		if len(h.semd.reqs) == 0 {
			t.Fatal("no search")
		}
		return h.semd.reqs[len(h.semd.reqs)-1]
	}
	if r := last(); r.K != defaultLimit || r.Kinds != nil || r.Roots != nil {
		t.Fatalf("default search %+v", r)
	}

	// Each choice searches again at once, without the typing pause.
	n := len(h.semd.reqs)
	h.chooseFrom("All Kinds", "PDF")
	if len(h.semd.reqs) != n+1 || !slices.Equal(last().Kinds, []string{"pdf"}) {
		t.Fatalf("after PDF: %d searches, %+v", len(h.semd.reqs)-n, last())
	}
	h.chooseFrom("PDF", "Text")
	if !slices.Equal(last().Kinds, []string{"text", "pdf"}) {
		t.Fatalf("kinds %v", last().Kinds)
	}
	h.want("Text, PDF")
	for _, k := range []string{"Office", "Images", "Audio"} {
		h.chooseFrom(h.a.kindsLabel(), k)
	}
	h.want("5 Kinds")
	h.chooseFrom("5 Kinds", "Video") // every kind is all of them
	if last().Kinds != nil || h.a.kindsLabel() != "All Kinds" {
		t.Fatalf("all kinds: %v, %q", last().Kinds, h.a.kindsLabel())
	}
	h.chooseFrom("All Kinds", "Audio")
	h.chooseFrom("Audio", "Audio") // off again: none is all
	if last().Kinds != nil {
		t.Fatalf("kinds %v", last().Kinds)
	}

	h.chooseFrom("All Folders", "~/Work")
	if !slices.Equal(last().Roots, []string{"/Users/me/Work"}) {
		t.Fatalf("roots %v", last().Roots)
	}
	h.want("Work")
	h.chooseFrom("50 Results", "100 Results")
	if last().K != 100 {
		t.Fatalf("k %d", last().K)
	}
	h.want("Reset")
	h.want("No matches in the kinds and folder chosen.")
	h.maybeShot("filters")

	// A folder semd stops indexing stops narrowing the search.
	h.semd.status.Roots = h.semd.status.Roots[:1]
	n = len(h.semd.reqs)
	h.Advance(statusEvery)
	h.flush()
	if h.a.root != "" || len(h.semd.reqs) != n+1 || last().Roots != nil {
		t.Fatalf("root %q, roots %v", h.a.root, last().Roots)
	}
	h.chooseFrom("All Kinds", "Video")
	h.click("Reset")
	h.flush()
	if last().Kinds != nil || last().K != 100 || h.HasText("Reset") {
		t.Fatalf("after reset %+v", last())
	}

	// With no query, a choice searches nothing.
	h.a.query = ""
	h.a.queryChanged(h.Now())
	n = len(h.semd.reqs)
	h.chooseFrom("100 Results", "10 Results")
	if len(h.semd.reqs) != n {
		t.Fatal("searched an empty query")
	}
}

func TestActivity(t *testing.T) {
	h := newHarness(t)
	h.a.svc = &fakeService{}
	at := time.Date(2026, 10, 27, 9, 30, 0, 0, time.Local)
	var evs []events.Event
	for i := range 25 {
		evs = append(evs, events.Event{Time: at.Add(time.Duration(i) * time.Second), Type: "indexed", Path: fmt.Sprintf("/Users/me/doc %02d.txt", i)})
	}
	evs = append(evs, events.Event{Time: at.Add(time.Minute), Type: "failed", Path: "/Users/me/bad.pdf", Msg: "broken xref"},
		events.Event{Time: at.Add(time.Minute), Type: "brand-new", Msg: "from a newer semd"})
	h.a.addActivity(evs)
	h.click("Settings")
	h.flush()
	h.click("Activity")
	h.want("from a newer semd") // newest first; unknown types show as they are
	h.want("brand-new")
	h.want("Failed")
	h.want("09:31:00")
	h.want("1–8 of 27")
	if h.HasText("/Users/me/doc 00.txt") {
		t.Fatal("oldest on the first page")
	}

	// Off the first page the log holds still.
	h.click("Next activity")
	h.want("9–16 of 27")
	h.a.addActivity([]events.Event{{Time: at.Add(2 * time.Minute), Type: "removed", Path: "/Users/me/new"}})
	h.Frame()
	h.want("9–16 of 27")
	h.want("Held: newer events are not shown")
	h.click("Back to Live")
	h.want("1–8 of 28")
	h.want("Removed")
	h.click("Clear")
	h.want("Nothing yet: semd's work shows here as it happens.")

	// It keeps the last maxActivity.
	evs = nil
	for i := range maxActivity + 7 {
		evs = append(evs, events.Event{Type: "indexed", Path: fmt.Sprint(i)})
	}
	h.a.addActivity(evs[:3])
	h.a.addActivity(evs[3:])
	if len(h.a.activity) != maxActivity || h.a.activity[0].Path != "7" {
		t.Fatalf("%d kept, oldest %q", len(h.a.activity), h.a.activity[0].Path)
	}
	var q activityQueue
	for _, ev := range evs {
		q.add(ev)
	}
	if got := q.take(); len(got) < maxActivity || got[len(got)-1].Path != evs[len(evs)-1].Path {
		t.Fatalf("queue kept %d", len(got))
	}
	if len(q.take()) != 0 {
		t.Fatal("take left events")
	}
}

func TestService(t *testing.T) {
	h := newHarness(t)
	svc := &fakeService{info: agent.Info{Loaded: true, State: "running", PID: 42, Runs: 2, LastExit: "0", Program: "/x/semd"}}
	h.a.svc = svc
	h.a.agentErr = "launchctl bootstrap: exit 5"
	h.click("Settings")
	h.flush()
	h.click("Service")
	h.flush()
	h.want("semd: running (pid 42)")
	h.want("Started 2 times since loaded · last exit: 0")
	h.want("Log: ~/Library/Logs/Kim Quy/dev.kimquy.test.log")

	h.click("Stop semd")
	h.want("Stopping semd…")
	h.flush()
	h.flush() // the info asked for after the action
	if svc.stops != 1 {
		t.Fatalf("%d stops", svc.stops)
	}
	h.want("semd: stopped")
	if h.HasText("Program: /x/semd") {
		t.Fatal("details of a stopped agent")
	}
	h.click("Restart semd") // disabled while stopped
	h.flush()
	if svc.restarts != 0 {
		t.Fatal("restarted a stopped agent")
	}
	h.click("Start semd")
	h.flush()
	h.flush()
	h.want("semd: running (pid 42)")
	if svc.starts != 1 || h.a.agentErr != "" {
		t.Fatalf("starts %d, agentErr %q", svc.starts, h.a.agentErr)
	}
	svc.fail = errors.New("launchctl kickstart: exit 5")
	h.click("Restart semd")
	h.flush()
	h.want("Restarting semd failed: launchctl kickstart: exit 5")
	h.click("Open Log")
	if !slices.Equal(h.shell.opened, []string{svc.LogPath()}) {
		t.Fatalf("opened %v", h.shell.opened)
	}

	// It is asked again every infoEvery while it shows, and not otherwise.
	h.Advance(infoEvery)
	h.Frame()
	if len(h.queue) == 0 {
		t.Fatal("no info request while shown")
	}
	h.flush()
	h.click("Folders")
	h.Advance(2 * infoEvery)
	h.Frame()
	if h.a.infoBusy {
		t.Fatal("info asked while hidden")
	}

	// Without a service the tab says how to control semd instead.
	h2 := newHarness(t)
	h2.click("Settings")
	h2.flush()
	h2.click("Service")
	h2.want("Kim Quy does not run semd here (it runs from Kim Quy.app only): control it with semctl and launchctl.")
	if h2.HasText("Uninstall Kim Quy…") {
		t.Fatal("uninstall without a service")
	}
}

func TestUninstallFlow(t *testing.T) {
	h := newHarness(t)
	svc := &fakeService{info: agent.Info{Loaded: true, State: "running"},
		removed: []string{"Login Items entry", "LaunchAgent dev.kimquy.test"}, uninstallErr: errors.New("delete the index: a semd still uses it")}
	h.a.svc = svc
	h.click("Settings")
	h.flush()
	h.click("Service")
	h.flush()
	h.click("Uninstall Kim Quy…")
	h.want("Uninstall Kim Quy?")
	h.click("Cancel")
	if svc.uninstalls != 0 || h.a.askUninstall {
		t.Fatal("canceled, yet uninstalled")
	}
	h.click("Uninstall Kim Quy…")
	h.maybeShot("uninstall-ask")
	h.click("Uninstall")
	h.want("Uninstalling Kim Quy…")
	if h.a.settingsOpen {
		t.Fatal("settings still open")
	}
	h.Key(ui.Cmd, ui.KeyComma) // settings stay closed meanwhile
	h.flush()
	h.want("Kim Quy is not completely uninstalled")
	h.maybeShot("uninstall-failed")
	h.want("delete the index: a semd still uses it")
	h.want("• LaunchAgent dev.kimquy.test")
	h.click("Try Again")
	svc.uninstallErr = nil
	svc.removed = append(svc.removed, "index and settings: ~/Library/Application Support/Kim Quy")
	h.flush()
	h.want("Kim Quy is uninstalled")
	h.maybeShot("uninstall-done")
	h.want("• index and settings: ~/Library/Application Support/Kim Quy")
	if svc.uninstalls != 2 {
		t.Fatalf("%d uninstalls", svc.uninstalls)
	}
	// The last screen cannot be dismissed: only quit.
	h.Key(0, ui.KeyEscape)
	h.want("Kim Quy is uninstalled")
	h.click("Open Privacy Settings")
	h.click("Quit")
	if h.shell.quit != 1 || h.shell.privacy != 1 {
		t.Fatalf("quit %d, privacy %d", h.shell.quit, h.shell.privacy)
	}
}

// fakeTrail is a home with everything Kim Quy leaves, in a temp dir.
type fakeTrail struct {
	t                       *testing.T
	home, state, app, cache string
	calls                   []string
	u                       *uninstaller
}

func newFakeTrail(t *testing.T) *fakeTrail {
	root := t.TempDir()
	f := &fakeTrail{t: t, home: filepath.Join(root, "home"), cache: filepath.Join(root, "C")}
	f.state = filepath.Join(f.home, "Library", "Application Support", "Kim Quy")
	f.app = filepath.Join(root, "Applications", "Kim Quy.app")
	lib := filepath.Join(f.home, "Library")
	for _, d := range []string{
		filepath.Join(f.state, "catalog"), filepath.Join(f.state, "store"),
		filepath.Join(lib, "Logs", "Kim Quy"), filepath.Join(lib, "Caches", agent.BundleID),
		filepath.Join(lib, "Saved Application State", agent.BundleID+".savedState"),
		filepath.Join(f.cache, agent.BundleID), filepath.Join(f.app, "Contents"),
		filepath.Join(lib, "Caches", "com.other.app"),
	} {
		mustMkdir(t, d)
	}
	for p, data := range map[string]string{
		filepath.Join(f.state, "config.json"):                      "{}",
		filepath.Join(f.state, "watch.json.tmp"):                   "{}",
		filepath.Join(f.state, "LOCK"):                             "",
		filepath.Join(f.state, "store", "seg-1"):                   "x",
		filepath.Join(lib, "Logs", "Kim Quy", "a.log"):             "log",
		filepath.Join(lib, "Preferences", agent.BundleID+".plist"): "p",
		filepath.Join(lib, "Preferences", "com.other.app.plist"):   "p",
		filepath.Join(f.app, "Contents", "Info.plist"):             "<key>CFBundleIdentifier</key><string>" + agent.BundleID + "</string>",
	} {
		mustMkdir(t, filepath.Dir(p))
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rec := func(s string) { f.calls = append(f.calls, s) }
	f.u = &uninstaller{
		label: "dev.kimquy.test", bundleID: agent.BundleID,
		state: f.state, home: f.home, app: f.app, userCache: f.cache,
		removeAgent:    func(l string) error { rec("agent " + l); return nil },
		setOpenAtLogin: func(on bool) error { rec(fmt.Sprint("login ", on)); return nil },
		run: func(name string, args ...string) (int, string, error) {
			rec(filepath.Base(name) + " " + strings.Join(args, " "))
			return 0, "", nil
		},
		trash: func(p string) error { rec("trash " + p); return os.RemoveAll(p) },
		sweep: func(paths []string) error { rec(fmt.Sprintf("sweep %d", len(paths))); return nil },
	}
	return f
}

func mustMkdir(t *testing.T, d string) {
	t.Helper()
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestUninstaller(t *testing.T) {
	f := newFakeTrail(t)
	removed, err := f.u.Uninstall()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"login false", "agent dev.kimquy.test", "defaults delete " + agent.BundleID,
		"tccutil reset All " + agent.BundleID, "trash " + f.app, "sweep 7"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls\n%q\nwant\n%q", f.calls, want)
	}
	lib := filepath.Join(f.home, "Library")
	for _, p := range []string{f.state, filepath.Join(lib, "Logs", "Kim Quy"), filepath.Join(lib, "Preferences", agent.BundleID+".plist"),
		filepath.Join(lib, "Caches", agent.BundleID), filepath.Join(lib, "Saved Application State", agent.BundleID+".savedState"),
		filepath.Join(f.cache, agent.BundleID), f.app} {
		if exists(p) {
			t.Errorf("%s is left", p)
		}
	}
	for _, p := range []string{filepath.Join(lib, "Caches", "com.other.app"), filepath.Join(lib, "Preferences", "com.other.app.plist")} {
		if !exists(p) {
			t.Errorf("%s, not Kim Quy's, is gone", p)
		}
	}
	if len(removed) != 10 || !strings.HasPrefix(removed[2], "index and settings: ") {
		t.Fatalf("removed %q", removed)
	}

	// Again, with nothing left: it does what it can and fails on the app.
	f.calls = nil
	if removed, err := f.u.Uninstall(); err == nil || len(f.calls) != 0 || len(removed) != 0 {
		t.Fatalf("second run: %q, %v, calls %q", removed, err, f.calls)
	}
}

func TestUninstallerRefuses(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(f *fakeTrail)
		want  string
		calls int // made before it stopped
	}{
		{"foreign file in state", func(f *fakeTrail) {
			os.WriteFile(filepath.Join(f.state, "thesis.docx"), nil, 0o600)
		}, "which semd did not make", 0},
		{"state is home", func(f *fakeTrail) { f.u.state = f.home }, "holds your home folder", 0},
		{"state above home", func(f *fakeTrail) { f.u.state = filepath.Dir(f.home) }, "holds your home folder", 0},
		{"state is root", func(f *fakeTrail) { f.u.state = "/" }, "holds your home folder", 0},
		{"state is a symlink", func(f *fakeTrail) {
			os.RemoveAll(f.state)
			os.Symlink(f.home, f.state)
		}, "not a directory", 0},
		{"relative state", func(f *fakeTrail) { f.u.state = "Kim Quy" }, "not a clean absolute path", 0},
		{"not our app", func(f *fakeTrail) {
			os.WriteFile(filepath.Join(f.app, "Contents", "Info.plist"), []byte("<string>com.other.app</string>"), 0o600)
		}, "is not " + agent.BundleID, 0},
		{"not an app", func(f *fakeTrail) { f.u.app = filepath.Dir(f.app) }, "is not an app", 0},
		{"bad bundle ID", func(f *fakeTrail) { f.u.bundleID = "../x" }, "bad label", 0},
		{"no dependency", func(f *fakeTrail) { f.u.trash = nil }, "nil dependency", 0},
		{"agent fails", func(f *fakeTrail) {
			f.u.removeAgent = func(string) error { f.calls = append(f.calls, "agent"); return errors.New("bootout: exit 5") }
		}, "stop semd: bootout: exit 5", 2},
		{"defaults fails", func(f *fakeTrail) {
			f.u.run = func(string, ...string) (int, string, error) { f.calls = append(f.calls, "run"); return 1, "boom", nil }
		}, "defaults: exit 1: boom", 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeTrail(t)
			c.setup(f)
			_, err := f.u.Uninstall()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want %q", err, c.want)
			}
			if len(f.calls) != c.calls {
				t.Fatalf("calls %q, want %d", f.calls, c.calls)
			}
			if c.calls == 0 && !exists(filepath.Join(f.home, "Library", "Logs", "Kim Quy", "a.log")) {
				t.Fatal("deleted before refusing")
			}
		})
	}

	// A semd that still holds the index keeps it, and the app.
	f := newFakeTrail(t)
	lock, err := lockDir(f.state)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := f.u.Uninstall(); err == nil || !strings.Contains(err.Error(), "a semd still uses it") {
		t.Fatalf("locked: %v", err)
	}
	if !exists(filepath.Join(f.state, "config.json")) || !exists(f.app) {
		t.Fatal("deleted a locked index, or went on")
	}

	// Logs not Kim Quy's stay, with their folder.
	f = newFakeTrail(t)
	other := filepath.Join(f.home, "Library", "Logs", "Kim Quy", "notes.txt")
	os.WriteFile(other, nil, 0o600)
	if _, err := f.u.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if !exists(other) || exists(filepath.Join(filepath.Dir(other), "a.log")) {
		t.Fatal("logs: wrong files deleted")
	}
}

var lockDir = fsutil.LockDir

// maybeShot saves a picture of the window as <SEMSEARCH_SHOT>-name.png.
func (h *harness) maybeShot(name string) {
	if out := os.Getenv("SEMSEARCH_SHOT"); out != "" {
		h.shot(out + "-" + name + ".png")
	}
}

// The sweeper waits for the process to exit, then deletes the paths, which
// may have spaces and leading dashes: they are arguments, not script.
func TestSweepScript(t *testing.T) {
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "Saved Application State"), filepath.Join(dir, "-rf x.plist")}
	keep := filepath.Join(dir, "keep")
	for _, p := range append(paths, keep) {
		mustMkdir(t, p)
	}
	waited := exec.Command("/bin/sleep", "1")
	if err := waited.Start(); err != nil {
		t.Fatal(err)
	}
	go waited.Wait()
	start := time.Now()
	args := append([]string{"-c", sweepScript, "sweep", fmt.Sprint(waited.Process.Pid), "dev.kimquy.test-sweep"}, paths...)
	if out, err := exec.Command("/bin/sh", args...).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Fatal("did not wait for the process")
	}
	for _, p := range paths {
		if exists(p) {
			t.Errorf("%s is left", p)
		}
	}
	if !exists(keep) {
		t.Fatal("deleted a path not given")
	}
}

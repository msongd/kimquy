package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func spec(dir string) Spec {
	return Spec{
		Label:      "dev.kimquy.test",
		Program:    "/Applications/Sem Search.app/Contents/Helpers/semd",
		Args:       []string{"-state", dir, "-x=<a & b>"},
		WorkingDir: dir,
		LogPath:    filepath.Join(dir, "semd.log"),
		Env:        map[string]string{"B": "2", "A": `"1"`},
		BundleID:   "dev.kimquy.app",
	}
}

// TestPlistParses checks the plist with Apple's own parser.
func TestPlistParses(t *testing.T) {
	dir := t.TempDir()
	s := spec(dir)
	b, err := s.Plist()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "a.plist")
	os.WriteFile(p, b, 0o600)
	if out, err := exec.Command("plutil", "-lint", p).CombinedOutput(); err != nil {
		t.Fatalf("lint: %v %s\n%s", err, out, b)
	}
	out, err := exec.Command("plutil", "-convert", "json", "-o", "-", p).Output()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"Label":                       s.Label,
		"ProgramArguments":            []any{s.Program, "-state", dir, "-x=<a & b>"},
		"WorkingDirectory":            dir,
		"StandardOutPath":             s.LogPath,
		"StandardErrorPath":           s.LogPath,
		"RunAtLoad":                   true,
		"KeepAlive":                   map[string]any{"SuccessfulExit": false},
		"ThrottleInterval":            10.0,
		"AssociatedBundleIdentifiers": []any{"dev.kimquy.app"},
		"EnvironmentVariables":        map[string]any{"A": `"1"`, "B": "2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plist:\n got %v\nwant %v", got, want)
	}
	again, _ := s.Plist()
	if string(again) != string(b) {
		t.Fatal("plist is not deterministic")
	}
}

func TestSpecRejected(t *testing.T) {
	dir := t.TempDir()
	for name, mut := range map[string]func(*Spec){
		"label":     func(s *Spec) { s.Label = "a/b" },
		"empty":     func(s *Spec) { s.Label = "" },
		"relative":  func(s *Spec) { s.Program = "semd" },
		"unclean":   func(s *Spec) { s.WorkingDir = dir + "/../x" },
		"log":       func(s *Spec) { s.LogPath = "" },
		"env name":  func(s *Spec) { s.Env = map[string]string{"A=B": "x"} },
		"nul arg":   func(s *Spec) { s.Args = []string{"a\x00b"} },
		"ctrl arg":  func(s *Spec) { s.Args = []string{"a\x01b"} },
		"bad utf8":  func(s *Spec) { s.Args = []string{"a\xffb"} },
		"env value": func(s *Spec) { s.Env = map[string]string{"A": "\x02"} },
		"bundle id": func(s *Spec) { s.BundleID = "\x03" },
	} {
		s := spec(dir)
		mut(&s)
		if _, err := s.Plist(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// fakeLaunchctl records calls and answers like launchctl for one service.
type fakeLaunchctl struct {
	calls    []string
	loaded   bool
	failBoot int // bootstrap fails this many times first
	lingers  int // after a bootout, print still finds the service this many times
}

func (f *fakeLaunchctl) run(args ...string) (int, string, error) {
	f.calls = append(f.calls, args[0])
	switch args[0] {
	case "print":
		if f.lingers > 0 {
			f.lingers--
			return 0, "", nil
		}
		if f.loaded {
			return 0, "", nil
		}
		return exitNotFound, "Could not find service", nil
	case "bootout":
		if !f.loaded {
			return exitNoSuchProcess, "Boot-out failed: 3: No such process", nil
		}
		f.loaded = false
		return 0, "", nil
	case "kickstart":
		if !f.loaded {
			return exitNotFound, "Could not find service", nil
		}
		return 0, "", nil
	case "bootstrap":
		if f.failBoot > 0 {
			f.failBoot--
			return 5, "Bootstrap failed: 5: Input/output error", nil
		}
		f.loaded = true
		return 0, "", nil
	}
	return 1, "unknown", nil
}

func TestEnsure(t *testing.T) {
	defer func(d time.Duration) { retryDelay = d }(retryDelay)
	retryDelay = 0
	dir := t.TempDir()
	f := &fakeLaunchctl{}
	m := &Manager{Dir: filepath.Join(dir, "LaunchAgents"), Domain: "gui/1", Launchctl: f.run}
	s := spec(dir)

	started, err := m.Ensure(s)
	if err != nil || !started {
		t.Fatalf("first: %v %v", started, err)
	}
	fi, err := os.Stat(m.PlistPath(s.Label))
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("plist: %v %v", fi, err)
	}
	f.calls = nil
	if started, err = m.Ensure(s); err != nil || started {
		t.Fatalf("second: %v %v", started, err)
	}
	if strings.Join(f.calls, ",") != "print" {
		t.Fatalf("an installed agent was touched: %v", f.calls)
	}

	// A new binary hash changes the plist: restart.
	s.Env["SEMSEARCH_SEMD_SHA256"] = "new"
	f.calls = nil
	if started, err = m.Ensure(s); err != nil || !started {
		t.Fatalf("changed: %v %v", started, err)
	}
	if strings.Join(f.calls, ",") != "print,bootout,print,bootstrap" {
		t.Fatalf("calls: %v", f.calls)
	}

	// Unloaded by the user (launchctl bootout): load again.
	f.loaded = false
	f.failBoot = 2
	if started, err = m.Ensure(s); err != nil || !started || !f.loaded {
		t.Fatalf("reload: %v %v", started, err)
	}

	if err := m.Restart(s.Label); err != nil {
		t.Fatal(err)
	}
	st, err := m.Status(s)
	if err != nil || st != (Status{Plist: m.PlistPath(s.Label), Written: true, Current: true, Loaded: true}) {
		t.Fatalf("status %+v %v", st, err)
	}
	// Uninstall waits until launchd has let go of the service.
	f.lingers = 3
	f.calls = nil
	if err := m.Uninstall(s.Label); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.calls, ",") != "bootout,print,print,print,print" {
		t.Fatalf("uninstall calls: %v", f.calls)
	}
	if _, err := os.Stat(m.PlistPath(s.Label)); !os.IsNotExist(err) || f.loaded {
		t.Fatalf("after uninstall: %v loaded=%v", err, f.loaded)
	}
	if err := m.Uninstall(s.Label); err != nil {
		t.Fatalf("second uninstall: %v", err)
	}
}

func TestBootstrapFailsLoud(t *testing.T) {
	dir := t.TempDir()
	defer func(d time.Duration) { retryDelay = d }(retryDelay)
	retryDelay = 0
	f := &fakeLaunchctl{failBoot: 1000}
	m := &Manager{Dir: dir, Domain: "gui/1", Launchctl: f.run}
	err := m.Install(spec(dir))
	if err == nil || !strings.Contains(err.Error(), "Input/output error") {
		t.Fatalf("got %v", err)
	}
}

// TestLaunchd loads a real agent into this user's session under a test
// label and removes it again. SEMSEARCH_LAUNCHD=1 enables it.
func TestLaunchd(t *testing.T) {
	if os.Getenv("SEMSEARCH_LAUNCHD") != "1" {
		t.Skip("SEMSEARCH_LAUNCHD=1 runs it against the real launchd")
	}
	m, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "ran")
	s := Spec{
		Label:      fmt.Sprintf("dev.kimquy.test-%d", os.Getpid()),
		Program:    "/bin/sh",
		Args:       []string{"-c", `echo "$SEMSEARCH_TEST" > ran; sleep 600`},
		WorkingDir: dir,
		LogPath:    filepath.Join(dir, "log"),
		Env:        map[string]string{"SEMSEARCH_TEST": "hello"},
	}
	t.Cleanup(func() {
		if err := m.Uninstall(s.Label); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	if started, err := m.Ensure(s); err != nil || !started {
		t.Fatalf("ensure: %v %v", started, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, _ := os.ReadFile(marker)
		if string(b) == "hello\n" {
			break
		}
		if time.Now().After(deadline) {
			log, _ := os.ReadFile(s.LogPath)
			t.Fatalf("agent did not run; marker %q log %q", b, log)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if started, err := m.Ensure(s); err != nil || started {
		t.Fatalf("second ensure: %v %v", started, err)
	}
	os.Remove(marker)
	if err := m.Restart(s.Label); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(10 * time.Second)
	for b, _ := os.ReadFile(marker); string(b) != "hello\n"; b, _ = os.ReadFile(marker) {
		if time.Now().After(until) {
			t.Fatal("not restarted")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := m.Uninstall(s.Label); err != nil {
		t.Fatal(err)
	}
	if loaded, err := m.Loaded(s.Label); err != nil || loaded {
		t.Fatalf("still loaded: %v %v", loaded, err)
	}
}

func TestSemd(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "semd")
	os.WriteFile(exe, []byte("v1"), 0o755)
	s, err := Semd(DefaultLabel, exe, filepath.Join(dir, "state"), "")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(exe, []byte("v2"), 0o755)
	s2, err := Semd(DefaultLabel, exe, filepath.Join(dir, "state"), "")
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.Plist()
	b, _ := s2.Plist()
	if string(a) == string(b) {
		t.Fatal("a new binary leaves the plist unchanged")
	}
	os.Chmod(exe, 0o644)
	if _, err := Semd(DefaultLabel, exe, dir, ""); err == nil {
		t.Fatal("non-executable accepted")
	}
	if _, err := Semd(DefaultLabel, filepath.Join(dir, "missing"), dir, ""); err == nil {
		t.Fatal("missing program accepted")
	}
}

func TestUninstallStuck(t *testing.T) {
	defer func(d time.Duration) { retryDelay = d }(retryDelay)
	retryDelay = 0
	f := &fakeLaunchctl{loaded: true, lingers: 1000}
	m := &Manager{Dir: t.TempDir(), Domain: "gui/1", Launchctl: f.run}
	if err := m.Uninstall("dev.kimquy.test"); err == nil || !strings.Contains(err.Error(), "still loaded") {
		t.Fatalf("got %v", err)
	}
}

// printOut is trimmed `launchctl print` output: the service's fields are
// indented by one tab, nested blocks by more.
const printOut = "gui/501/dev.kimquy.semd = {\n" +
	"\tactive count = 1\n" +
	"\tpath = /Users/x/Library/LaunchAgents/dev.kimquy.semd.plist\n" +
	"\ttype = LaunchAgent\n" +
	"\tstate = running\n\n" +
	"\tprogram = /Applications/Kim Quy.app/Contents/Helpers/semd\n" +
	"\targuments = {\n" +
	"\t\t/Applications/Kim Quy.app/Contents/Helpers/semd\n" +
	"\t\tstate = nested, ignored\n" +
	"\t}\n\n" +
	"\truns = 3\n" +
	"\tpid = 6888\n" +
	"\tlast exit code = (never exited)\n" +
	"}\n"

func TestParsePrint(t *testing.T) {
	in, err := parsePrint(printOut)
	if err != nil {
		t.Fatal(err)
	}
	want := Info{Loaded: true, State: "running", PID: 6888, Runs: 3, LastExit: "(never exited)",
		Program: "/Applications/Kim Quy.app/Contents/Helpers/semd"}
	if in != want {
		t.Fatalf("got %+v, want %+v", in, want)
	}
	for _, bad := range []string{
		"",                               // no state
		"\tpid = 7\n",                    // still no state
		"\tstate = running\n\tpid = x\n", // pid is not a number
		"\tstate = running\n\truns = -\n",
	} {
		if in, err := parsePrint(bad); err == nil {
			t.Errorf("parsePrint(%q) = %+v, want an error", bad, in)
		}
	}
}

func TestInfoAndStop(t *testing.T) {
	answer := func(code int, out string, err error) func(...string) (int, string, error) {
		return func(...string) (int, string, error) { return code, out, err }
	}
	m := &Manager{Dir: t.TempDir(), Domain: "gui/1", Launchctl: answer(exitNotFound, "Could not find service", nil)}
	if in, err := m.Info(DefaultLabel); err != nil || in != (Info{}) {
		t.Fatalf("not loaded: %+v, %v", in, err)
	}
	m.Launchctl = answer(0, printOut, nil)
	if in, err := m.Info(DefaultLabel); err != nil || in.PID != 6888 {
		t.Fatalf("loaded: %+v, %v", in, err)
	}
	m.Launchctl = answer(5, "Input/output error", nil)
	if _, err := m.Info(DefaultLabel); err == nil {
		t.Fatal("exit 5: no error")
	}
	m.Launchctl = answer(0, "", errors.New("no launchctl"))
	if _, err := m.Info(DefaultLabel); err == nil {
		t.Fatal("launchctl missing: no error")
	}
	if _, err := m.Info("../x"); err == nil {
		t.Fatal("bad label: no error")
	}
	if err := m.Stop("a b"); err == nil {
		t.Fatal("Stop: bad label: no error")
	}

	// Stop unloads and keeps the plist.
	f := &fakeLaunchctl{loaded: true}
	m = &Manager{Dir: t.TempDir(), Domain: "gui/1", Launchctl: f.run}
	plist := m.PlistPath(DefaultLabel)
	if err := os.WriteFile(plist, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(DefaultLabel); err != nil {
		t.Fatal(err)
	}
	if f.loaded {
		t.Fatal("still loaded")
	}
	if _, err := os.Stat(plist); err != nil {
		t.Fatalf("plist gone: %v", err)
	}
	if err := m.Stop(DefaultLabel); err != nil {
		t.Fatalf("Stop when stopped: %v", err)
	}
}

func TestLogPath(t *testing.T) {
	if got, want := LogPath("/h", "a.b"), "/h/Library/Logs/Kim Quy/a.b.log"; got != want {
		t.Fatalf("%s != %s", got, want)
	}
}

// Package agent installs semd as a per-user LaunchAgent: it writes the
// plist to ~/Library/LaunchAgents and loads it with launchctl in the user's
// GUI domain, so launchd starts semd at login and restarts it if it fails.
package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/msongd/kimquy/internal/fsutil"
)

const (
	// DefaultLabel is the launchd label of the installed daemon.
	DefaultLabel = "dev.kimquy.semd"
	// BundleID is Kim Quy.app's CFBundleIdentifier (packaging/build.sh).
	BundleID = "dev.kimquy.app"
)

// Spec describes the agent.
type Spec struct {
	Label      string
	Program    string            // absolute path of the executable
	Args       []string          // arguments after the program
	WorkingDir string            // absolute
	LogPath    string            // absolute; stdout and stderr go here
	Env        map[string]string // extra environment
	BundleID   string            // optional: the app that owns it, shown in Login Items
}

var labelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,127}$`)

// Validate checks that s can be written and loaded.
func (s Spec) Validate() error {
	if !labelRE.MatchString(s.Label) {
		return fmt.Errorf("agent: bad label %q", s.Label)
	}
	for name, p := range map[string]string{"program": s.Program, "working directory": s.WorkingDir, "log path": s.LogPath} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return fmt.Errorf("agent: %s %q is not a clean absolute path", name, p)
		}
	}
	for k := range s.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			return fmt.Errorf("agent: bad environment name %q", k)
		}
	}
	return nil
}

// Plist renders s as a launchd property list. Output is deterministic, so
// an installed agent can be compared byte for byte.
func (s Spec) Plist() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	str := func(v string) {
		b.WriteString("<string>")
		xml.EscapeText(&b, []byte(v))
		b.WriteString("</string>")
	}
	key := func(k string) {
		b.WriteString("\t<key>")
		xml.EscapeText(&b, []byte(k))
		b.WriteString("</key>")
	}
	for _, v := range slices.Concat(s.Args, []string{s.Program, s.WorkingDir, s.LogPath, s.BundleID}) {
		if !validXML(v) {
			return nil, fmt.Errorf("agent: value %q cannot be stored in a plist", v)
		}
	}
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	key("Label")
	str(s.Label)
	b.WriteString("\n")
	key("ProgramArguments")
	b.WriteString("<array>")
	for _, a := range append([]string{s.Program}, s.Args...) {
		str(a)
	}
	b.WriteString("</array>\n")
	key("WorkingDirectory")
	str(s.WorkingDir)
	b.WriteString("\n")
	key("StandardOutPath")
	str(s.LogPath)
	b.WriteString("\n")
	key("StandardErrorPath")
	str(s.LogPath)
	b.WriteString("\n")
	key("RunAtLoad")
	b.WriteString("<true/>\n")
	// Restart after a crash or an error exit, not after a clean stop.
	key("KeepAlive")
	b.WriteString("<dict><key>SuccessfulExit</key><false/></dict>\n")
	key("ThrottleInterval")
	b.WriteString("<integer>10</integer>\n")
	if s.BundleID != "" {
		key("AssociatedBundleIdentifiers")
		b.WriteString("<array>")
		str(s.BundleID)
		b.WriteString("</array>\n")
	}
	if len(s.Env) > 0 {
		key("EnvironmentVariables")
		b.WriteString("<dict>")
		for _, k := range slices.Sorted(maps.Keys(s.Env)) {
			if !validXML(k) || !validXML(s.Env[k]) {
				return nil, fmt.Errorf("agent: environment %q cannot be stored in a plist", k)
			}
			b.WriteString("<key>")
			xml.EscapeText(&b, []byte(k))
			b.WriteString("</key>")
			str(s.Env[k])
		}
		b.WriteString("</dict>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes(), nil
}

// validXML reports whether v has only characters XML 1.0 allows.
func validXML(v string) bool {
	for _, r := range v {
		if r == '�' || !(r == 0x9 || r == 0xA || r == 0xD || r >= 0x20 && r <= 0xD7FF || r >= 0xE000 && r <= 0xFFFD || r >= 0x10000 && r <= 0x10FFFF) {
			return false
		}
	}
	return true
}

// FileHash is the SHA-256 of the file at path, hex encoded. Putting the
// program's hash in the agent's environment makes an upgraded binary change
// the plist, so Ensure restarts the daemon.
func FileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Manager installs agents into Dir in a launchd domain.
type Manager struct {
	Dir    string // the LaunchAgents directory
	Domain string // e.g. gui/501
	// Launchctl runs launchctl; tests replace it.
	Launchctl func(args ...string) (exitCode int, output string, err error)
}

// Default is the manager of the current user's GUI session.
func Default() (*Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Manager{
		Dir:       filepath.Join(home, "Library", "LaunchAgents"),
		Domain:    fmt.Sprintf("gui/%d", os.Getuid()),
		Launchctl: runLaunchctl,
	}, nil
}

func runLaunchctl(args ...string) (int, string, error) {
	out, err := exec.Command("/bin/launchctl", args...).CombinedOutput()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode(), string(out), nil
	}
	if err != nil {
		return -1, string(out), err
	}
	return 0, string(out), nil
}

// retryDelay spaces bootstrap attempts.
var retryDelay = 250 * time.Millisecond

// launchctl exit codes for "no such service".
const (
	exitNoSuchProcess = 3   // bootout
	exitNotFound      = 113 // print
)

// PlistPath is where the agent with label is written.
func (m *Manager) PlistPath(label string) string {
	return filepath.Join(m.Dir, label+".plist")
}

func (m *Manager) run(args ...string) error {
	code, out, err := m.Launchctl(args...)
	if err != nil {
		return fmt.Errorf("launchctl %s: %w", strings.Join(args, " "), err)
	}
	if code != 0 {
		return fmt.Errorf("launchctl %s: exit %d: %s", strings.Join(args, " "), code, strings.TrimSpace(out))
	}
	return nil
}

// Loaded reports whether launchd knows the service.
func (m *Manager) Loaded(label string) (bool, error) {
	code, out, err := m.Launchctl("print", m.Domain+"/"+label)
	switch {
	case err != nil:
		return false, fmt.Errorf("launchctl print: %w", err)
	case code == 0:
		return true, nil
	case code == exitNotFound:
		return false, nil
	}
	return false, fmt.Errorf("launchctl print %s/%s: exit %d: %s", m.Domain, label, code, strings.TrimSpace(out))
}

// Status describes an installed agent.
type Status struct {
	Plist   string // path of the plist
	Written bool   // the plist exists
	Current bool   // and matches the spec
	Loaded  bool   // launchd knows the service
}

// Status compares what is installed with s.
func (m *Manager) Status(s Spec) (Status, error) {
	want, err := s.Plist()
	if err != nil {
		return Status{}, err
	}
	st := Status{Plist: m.PlistPath(s.Label)}
	have, err := os.ReadFile(st.Plist)
	switch {
	case err == nil:
		st.Written = true
		st.Current = bytes.Equal(have, want)
	case !errors.Is(err, os.ErrNotExist):
		return st, err
	}
	st.Loaded, err = m.Loaded(s.Label)
	return st, err
}

// Ensure installs s unless it is already installed and loaded. It reports
// whether it (re)started the agent.
func (m *Manager) Ensure(s Spec) (bool, error) {
	st, err := m.Status(s)
	if err != nil {
		return false, err
	}
	if st.Current && st.Loaded {
		return false, nil
	}
	return true, m.Install(s)
}

// Install writes the plist and (re)loads the agent, which starts it.
func (m *Manager) Install(s Spec) error {
	b, err := s.Plist()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return err
	}
	// launchd cannot start a job whose working directory is missing.
	for _, d := range []string{filepath.Dir(s.LogPath), s.WorkingDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	if err := m.bootout(s.Label); err != nil {
		return err
	}
	p := m.PlistPath(s.Label)
	if err := fsutil.WriteFileAtomic(p, b); err != nil {
		return err
	}
	// launchd ignores agent plists that others can write.
	if err := os.Chmod(p, 0o644); err != nil {
		return err
	}
	// Right after a bootout the old instance may still be going away, and
	// bootstrap then fails with EIO; give it a few seconds.
	var last error
	for range 20 {
		if last = m.run("bootstrap", m.Domain, p); last == nil {
			return nil
		}
		time.Sleep(retryDelay)
	}
	return last
}

// bootout unloads the service (which stops it) if it is loaded.
func (m *Manager) bootout(label string) error {
	code, out, err := m.Launchctl("bootout", m.Domain+"/"+label)
	switch {
	case err != nil:
		return fmt.Errorf("launchctl bootout: %w", err)
	case code == exitNoSuchProcess:
		return nil
	case code != 0:
		return fmt.Errorf("launchctl bootout %s/%s: exit %d: %s", m.Domain, label, code, strings.TrimSpace(out))
	}
	// launchd removes the service once the job has exited, which can take
	// until its exit timeout (20 s) when semd is slow to stop.
	for range 120 {
		loaded, err := m.Loaded(label)
		if err != nil || !loaded {
			return err
		}
		time.Sleep(retryDelay)
	}
	return fmt.Errorf("launchctl bootout %s/%s: service still loaded after 30 s", m.Domain, label)
}

// Info is what launchd says about a service.
type Info struct {
	Loaded   bool   // launchd knows the service; the rest is unset when not
	State    string // e.g. "running", "not running"
	PID      int    // 0 when not running
	Runs     int    // times launchd started it since it was loaded
	LastExit string // e.g. "0", "(never exited)"
	Program  string
}

// Info asks launchd about the service.
func (m *Manager) Info(label string) (Info, error) {
	if !labelRE.MatchString(label) {
		return Info{}, fmt.Errorf("agent: bad label %q", label)
	}
	code, out, err := m.Launchctl("print", m.Domain+"/"+label)
	switch {
	case err != nil:
		return Info{}, fmt.Errorf("launchctl print: %w", err)
	case code == exitNotFound:
		return Info{}, nil
	case code != 0:
		return Info{}, fmt.Errorf("launchctl print %s/%s: exit %d: %s", m.Domain, label, code, strings.TrimSpace(out))
	}
	return parsePrint(out)
}

// parsePrint reads the service's own fields (indented by one tab) out of
// launchctl print, whose format is not an API: it fails on a missing state
// rather than guess.
func parsePrint(out string) (Info, error) {
	in := Info{Loaded: true}
	for _, l := range strings.Split(out, "\n") {
		if !strings.HasPrefix(l, "\t") || strings.HasPrefix(l, "\t\t") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(l, "\t"), " = ")
		if !ok {
			continue
		}
		var err error
		switch k {
		case "state":
			in.State = v
		case "pid":
			in.PID, err = strconv.Atoi(v)
		case "runs":
			in.Runs, err = strconv.Atoi(v)
		case "last exit code":
			in.LastExit = v
		case "program":
			in.Program = v
		}
		if err != nil {
			return Info{}, fmt.Errorf("launchctl print: %s: %w", k, err)
		}
	}
	if in.State == "" {
		return Info{}, errors.New("launchctl print: no state in its output")
	}
	return in, nil
}

// Stop unloads the agent, which stops it, and keeps its plist: launchd
// starts it again at the next login (and the app at its next launch).
func (m *Manager) Stop(label string) error {
	if !labelRE.MatchString(label) {
		return fmt.Errorf("agent: bad label %q", label)
	}
	return m.bootout(label)
}

// Restart stops the running agent's process, if any, and starts it again.
func (m *Manager) Restart(label string) error {
	if !labelRE.MatchString(label) {
		return fmt.Errorf("agent: bad label %q", label)
	}
	return m.run("kickstart", "-k", m.Domain+"/"+label)
}

// Uninstall stops the agent and removes its plist. It is not an error if
// it was not installed.
func (m *Manager) Uninstall(label string) error {
	if !labelRE.MatchString(label) {
		return fmt.Errorf("agent: bad label %q", label)
	}
	if err := m.bootout(label); err != nil {
		return err
	}
	if err := os.Remove(m.PlistPath(label)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// LogDir holds the logs of the semd agents.
func LogDir(home string) string { return filepath.Join(home, "Library", "Logs", "Kim Quy") }

// LogPath is the log of the semd agent with label.
func LogPath(home, label string) string { return filepath.Join(LogDir(home), label+".log") }

// Semd is the agent that runs the semd executable at path with the state
// directory state. bundleID may be empty.
func Semd(label, path, state, bundleID string) (Spec, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Spec{}, err
	}
	if path, err = filepath.Abs(path); err != nil {
		return Spec{}, err
	}
	if state, err = filepath.Abs(state); err != nil {
		return Spec{}, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return Spec{}, err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return Spec{}, fmt.Errorf("agent: %s is not an executable file", path)
	}
	sum, err := FileHash(path)
	if err != nil {
		return Spec{}, err
	}
	s := Spec{
		Label:      label,
		Program:    path,
		Args:       []string{"-state", state},
		WorkingDir: state,
		LogPath:    LogPath(home, label),
		Env:        map[string]string{"SEMSEARCH_SEMD_SHA256": sum},
		BundleID:   bundleID,
	}
	return s, s.Validate()
}

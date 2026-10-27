package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/msongd/kimquy/internal/agent"
	"github.com/msongd/kimquy/internal/fsutil"
)

// uninstaller removes Kim Quy and every trace of it: the LaunchAgent,
// the index and settings (semd's state directory), the logs, the app's
// preferences and caches, its macOS permissions and Login Items entry,
// and the app itself. Each step checks that what it deletes is what
// Kim Quy made, and it stops at the first failure: what is left stays
// in place for another try.
type uninstaller struct {
	label    string // of the LaunchAgent
	bundleID string
	state    string // semd's state directory
	home     string
	app      string // Kim Quy.app
	// userCache is the per-user cache directory under /var/folders
	// (getconf DARWIN_USER_CACHE_DIR), where macOS keeps some of an app's
	// caches.
	userCache string

	removeAgent    func(label string) error // unload it and delete its plist
	setOpenAtLogin func(bool) error
	run            func(name string, args ...string) (code int, out string, err error)
	trash          func(path string) error
	// sweep, after the app quits, deletes paths again: the app may write
	// its preferences or saved state once more on its way out.
	sweep func(paths []string) error
}

// stateEntries are what semd keeps in its state directory (see
// internal/engine and internal/fsutil); anything else means it is not
// only semd's, and is not deleted.
var stateEntries = []string{"catalog", "store", "models", "config.json", "watch.json", "LOCK", "semd.sock", ".DS_Store"}

func (u *uninstaller) check() error {
	for _, p := range []struct{ name, path string }{
		{"state", u.state}, {"home", u.home}, {"app", u.app}, {"user cache", u.userCache},
	} {
		if !filepath.IsAbs(p.path) || filepath.Clean(p.path) != p.path {
			return fmt.Errorf("uninstall: %s directory %q is not a clean absolute path", p.name, p.path)
		}
	}
	if !labelOK(u.label) || !labelOK(u.bundleID) {
		return fmt.Errorf("uninstall: bad label %q or bundle ID %q", u.label, u.bundleID)
	}
	if u.removeAgent == nil || u.setOpenAtLogin == nil || u.run == nil || u.trash == nil || u.sweep == nil {
		return errors.New("uninstall: nil dependency")
	}
	return nil
}

func labelOK(s string) bool {
	return s != "" && !strings.ContainsAny(s, "/ \x00") && !strings.HasPrefix(s, ".")
}

// Uninstall runs the steps in order and returns what it removed, for the
// user to see, even when a step fails.
func (u *uninstaller) Uninstall() ([]string, error) {
	if err := u.check(); err != nil {
		return nil, err
	}
	var removed []string
	done := func(what string) { removed = append(removed, what) }

	// Look before removing anything: a state directory that holds other
	// files is refused before semd is stopped.
	if err := u.checkState(); err != nil {
		return removed, err
	}
	if err := u.checkApp(); err != nil {
		return removed, err
	}
	if err := u.setOpenAtLogin(false); err != nil {
		return removed, fmt.Errorf("remove from Login Items: %w", err)
	}
	done("Login Items entry")
	if err := u.removeAgent(u.label); err != nil {
		return removed, fmt.Errorf("stop semd: %w", err)
	}
	done("LaunchAgent " + u.label)
	if ok, err := u.removeState(); err != nil {
		return removed, err
	} else if ok {
		done("index and settings: " + tildePath(u.state))
	}
	if ok, err := u.removeLogs(); err != nil {
		return removed, err
	} else if ok {
		done("logs: " + tildePath(agent.LogDir(u.home)))
	}
	trail := u.trail()
	for _, p := range trail {
		ok, err := removeIfExists(p)
		if err != nil {
			return removed, err
		}
		if ok {
			done(tildePath(p))
		}
	}
	if err := u.forgetPrefs(); err != nil {
		return removed, err
	}
	if err := u.resetPermissions(); err != nil {
		return removed, err
	}
	done("macOS permissions of " + u.bundleID)
	if err := u.trash(u.app); err != nil {
		return removed, fmt.Errorf("move %s to the Trash: %w", u.app, err)
	}
	done(tildePath(u.app) + " (moved to the Trash)")
	if err := u.sweep(trail); err != nil {
		return removed, fmt.Errorf("clean up after quitting: %w", err)
	}
	return removed, nil
}

// checkState makes sure the state directory, if any, is semd's alone: a
// real directory, not home or above it, holding only what semd makes.
func (u *uninstaller) checkState() error {
	s := u.state
	if s == "/" || s == u.home || strings.HasPrefix(u.home+"/", s+"/") {
		return fmt.Errorf("uninstall: refusing to delete %s, which holds your home folder", s)
	}
	fi, err := os.Lstat(s)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("uninstall: %w", err)
	case !fi.IsDir():
		return fmt.Errorf("uninstall: %s is not a directory (a symlink?): not deleted", s)
	}
	names, err := readNames(s)
	if err != nil {
		return err
	}
	for _, n := range names {
		if !slices.Contains(stateEntries, n) && !strings.HasSuffix(n, ".tmp") {
			return fmt.Errorf("uninstall: %s holds %q, which semd did not make: delete the folder yourself", s, n)
		}
	}
	return nil
}

// checkApp makes sure what goes to the Trash is Kim Quy.
func (u *uninstaller) checkApp() error {
	if !strings.HasSuffix(u.app, ".app") {
		return fmt.Errorf("uninstall: %s is not an app", u.app)
	}
	plist, err := os.ReadFile(filepath.Join(u.app, "Contents", "Info.plist"))
	if err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	if !bytes.Contains(plist, []byte("<string>"+u.bundleID+"</string>")) {
		return fmt.Errorf("uninstall: %s is not %s", u.app, u.bundleID)
	}
	return nil
}

// removeState deletes the state directory once no semd holds its lock.
func (u *uninstaller) removeState() (bool, error) {
	if _, err := os.Lstat(u.state); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err := u.checkState(); err != nil { // again: semd ran until now
		return false, err
	}
	lock, err := fsutil.LockDir(u.state)
	if err != nil {
		return false, fmt.Errorf("delete the index: a semd still uses it (%w); quit it and try again", err)
	}
	defer lock.Close()
	if err := os.RemoveAll(u.state); err != nil {
		return false, fmt.Errorf("delete the index: %w", err)
	}
	return true, nil
}

// removeLogs deletes the agents' logs, and their folder if nothing else is
// in it.
func (u *uninstaller) removeLogs() (bool, error) {
	dir := agent.LogDir(u.home)
	names, err := readNames(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, n := range names {
		if strings.HasSuffix(n, ".log") || n == ".DS_Store" {
			if err := os.Remove(filepath.Join(dir, n)); err != nil {
				return false, fmt.Errorf("delete the logs: %w", err)
			}
		}
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, syscall.ENOTEMPTY) {
		return false, fmt.Errorf("delete the logs: %w", err)
	}
	return true, nil
}

// trail is where macOS keeps an app's preferences, caches and state, all
// named by its bundle ID.
func (u *uninstaller) trail() []string {
	lib := filepath.Join(u.home, "Library")
	id := u.bundleID
	return []string{
		filepath.Join(lib, "Preferences", id+".plist"),
		filepath.Join(lib, "Caches", id),
		filepath.Join(lib, "Saved Application State", id+".savedState"),
		filepath.Join(lib, "HTTPStorages", id),
		filepath.Join(lib, "HTTPStorages", id+".binarycookies"),
		filepath.Join(lib, "WebKit", id),
		filepath.Join(u.userCache, id),
	}
}

// forgetPrefs drops the preferences cfprefsd holds in memory, which it
// would otherwise write back.
func (u *uninstaller) forgetPrefs() error {
	code, out, err := u.run("/usr/bin/defaults", "delete", u.bundleID)
	switch {
	case err != nil:
		return fmt.Errorf("delete the preferences: %w", err)
	case code != 0 && !strings.Contains(out, "does not exist"):
		return fmt.Errorf("delete the preferences: defaults: exit %d: %s", code, strings.TrimSpace(out))
	}
	return nil
}

// resetPermissions removes what the user allowed Kim Quy.app in Privacy
// & Security. semd's Full Disk Access cannot be: tccutil only takes
// bundle IDs.
func (u *uninstaller) resetPermissions() error {
	code, out, err := u.run("/usr/bin/tccutil", "reset", "All", u.bundleID)
	switch {
	case err != nil:
		return fmt.Errorf("reset permissions: %w", err)
	case code != 0 && !strings.Contains(out, "No such bundle identifier"):
		return fmt.Errorf("reset permissions: tccutil: exit %d: %s", code, strings.TrimSpace(out))
	}
	return nil
}

func readNames(dir string) ([]string, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	return names, nil
}

// removeIfExists deletes a file or directory, reporting whether it was
// there.
func removeIfExists(p string) (bool, error) {
	if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err := os.RemoveAll(p); err != nil {
		return false, err
	}
	return true, nil
}

// runCommand runs a program and returns its exit code and output; err is
// for when it could not run at all.
func runCommand(name string, args ...string) (int, string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), string(out), nil
	}
	if err != nil {
		return 0, "", err
	}
	return 0, string(out), nil
}

// sweepScript waits for the process $1 to exit (a minute at most), then
// deletes preferences of bundle ID $2 and the paths after them. The paths
// are arguments, never part of the script.
const sweepScript = `pid=$1 id=$2; shift 2
i=0; while kill -0 "$pid" 2>/dev/null && [ $i -lt 120 ]; do sleep 0.5; i=$((i+1)); done
/usr/bin/defaults delete "$id" >/dev/null 2>&1
rm -rf -- "$@"`

// sweepAfterExit starts sweepScript detached from this process, which is
// about to quit.
func sweepAfterExit(bundleID string, paths []string) error {
	args := append([]string{"-c", sweepScript, "sweep", fmt.Sprint(os.Getpid()), bundleID}, paths...)
	cmd := exec.Command("/bin/sh", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

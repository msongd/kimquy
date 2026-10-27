// Kim Quy is the search window of semd: type to search the contents of
// the indexed folders, Enter to open a result, Cmd-Enter to show it in
// Finder; Settings manages the folders, exclusions and indexing.
//
// It stays in the menu bar: closing the window (or Escape on an empty
// query) hides it, and the menu-bar icon or the global shortcut
// (Option-Shift-Space by default) brings it back. Quit is in the icon's menu.
//
//	cd ui && go run . [-socket path]
//
// It talks to semd over its socket; it needs no cgo. Run from
// Kim Quy.app, it first makes sure the bundled semd runs as a
// LaunchAgent (see internal/agent).
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/msongd/kimquy/internal/agent"
	"github.com/msongd/kimquy/internal/client"
	"github.com/msongd/kimquy/internal/config"
	"github.com/msongd/kimquy/internal/model"
)

// desktop is the shell of the running app.
type desktop struct{ win **mygo.Window }

func (d desktop) HideWindow() { hideWindow(*d.win) }

// showWindow and hideWindow keep the Dock icon in step with the window: while
// it is hidden only the menu-bar icon shows. An accessory app is not brought
// to the front by showing a window, hence App.Focus.
func showWindow(w *mygo.Window) {
	mygo.App.Dock.Show()
	w.Show()
	w.Focus()
	mygo.App.Focus()
}

func hideWindow(w *mygo.Window) {
	w.Hide()
	mygo.App.Dock.Hide()
}

func (desktop) Quit() { mygo.App.Quit() }

func (desktop) Open(path string) error { return mygo.Shell.OpenPath(path) }

func (desktop) Reveal(path string) error {
	// ShowItemInFolder reports nothing; a missing file would reveal nothing.
	if _, err := os.Lstat(path); err != nil {
		return err
	}
	mygo.Shell.ShowItemInFolder(path)
	return nil
}

// privacyURL opens System Settings at Privacy & Security › Full Disk Access.
const privacyURL = "x-apple.systempreferences:com.apple.settings.PrivacySecurity.extension?Privacy_AllFiles"

func (desktop) OpenPrivacySettings() error { return mygo.Shell.OpenExternal(privacyURL) }

func (d desktop) PickFolders() ([]string, error) {
	return mygo.Dialog.Open(mygo.OpenDialogOptions{
		Parent:            *d.win,
		Title:             "Add Folders to Search",
		ButtonLabel:       "Add",
		Directory:         true,
		Multiple:          true,
		CreateDirectories: false,
	})
}

func (d desktop) PickModel() (string, error) {
	got, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
		Parent:                     *d.win,
		Title:                      "Use a Model You Have",
		Message:                    "Choose the folder holding " + model.Parts[0].Dir + ", or an older Kim Quy.app.",
		ButtonLabel:                "Use",
		Directory:                  true,
		TreatPackagesAsDirectories: true, // an .app is a folder to choose
	})
	if err != nil || len(got) == 0 {
		return "", err
	}
	return got[0], nil
}

func main() {
	log.SetFlags(0)
	defState, err := config.DefaultStateDir()
	if err != nil {
		log.Fatal(err)
	}
	state := flag.String("state", defState, "semd state directory, for the LaunchAgent")
	socket := flag.String("socket", "", "semd socket (default <state>/semd.sock)")
	label := flag.String("label", agent.DefaultLabel, "launchd label of semd")
	install := flag.Bool("agent", true, "from the app bundle: install semd as a LaunchAgent if needed")
	shortcut := flag.String("shortcut", defaultShortcut, `global shortcut that shows or hides the window ("" for none)`)
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatalf("unexpected arguments: %v", flag.Args())
	}
	if *socket == "" {
		*socket = filepath.Join(*state, "semd.sock")
	}
	helper, err := bundledSemd()
	if err != nil {
		log.Fatal(err)
	}
	if len(*socket) >= 104 {
		log.Fatal(fmt.Errorf("socket path is %d bytes, the limit is 103: %s", len(*socket), *socket))
	}
	if !mygo.App.RequestSingleInstanceLock() {
		return // the running one comes to the front (OnSecondInstance)
	}
	mygo.App.SetName("Kim Quy")

	var win *mygo.Window
	ready := make(chan struct{})
	async := func(work func() func()) {
		go func() {
			apply := work()
			if apply == nil {
				return
			}
			<-ready // the view may ask before NewWindow returns
			win.Update(apply)
		}()
	}
	a := newApp(client.NewClient(*socket), desktop{&win}, async)
	go a.streamEvents(context.Background(), func(f func()) {
		<-ready
		win.Update(f)
	})
	a.helper = helper
	if helper != "" {
		a.mdl = newStateModels(helper, *state)
	}
	if helper != "" && *install {
		m, err := agent.Default()
		if err != nil {
			log.Fatal(err)
		}
		a.svc = &agentService{m: m, label: *label, semd: helper, state: *state}
		async(func() func() {
			// Without the model semd cannot start: the setup screen gets it
			// first, then starts semd.
			if have, err := a.mdl.Installed(); err != nil || !have[model.Parts[0].Dir] {
				return nil
			}
			err := ensureAgent(*label, helper, *state)
			if err != nil {
				log.Printf("semd LaunchAgent: %v", err)
			}
			return func() {
				if err != nil {
					a.agentErr = err.Error()
				}
			}
		})
	}
	show := func() {
		showWindow(win)
		a.input.Focus()
	}
	mygo.App.OnSecondInstance(func([]string, string) { show() })
	mygo.App.OnActivate(func(visible bool) { // the Dock icon
		if !visible {
			show()
		}
	})
	mygo.App.OnWindowAllClosed(func() {}) // the menu bar keeps it running
	mygo.App.WhenReady(func() {
		atLogin := mygo.App.WasOpenedAtLogin()
		if atLogin {
			mygo.App.Dock.Hide() // the window starts hidden
		}
		win = mygo.NewWindow(mygo.WindowOptions{
			Title:     "Kim Quy",
			Width:     760,
			Height:    520,
			MinWidth:  minWindowW, // the settings sheet fits, unscrolled
			MinHeight: minWindowH,
			Hidden:    atLogin,
			Content:   ui.View(a.view),
		})
		// Hide rather than close: the window comes back as it was. Quit
		// closes every window first and gives up if one refuses, so let
		// it through then.
		quitting := false
		mygo.App.OnBeforeQuit(func(*mygo.QuitEvent) { quitting = true })
		win.OnClose(func(e *mygo.CloseEvent) {
			if quitting {
				return
			}
			e.PreventDefault()
			hideWindow(win)
		})
		close(ready)
		// A hidden window draws no frames, so its view polls nothing: keep
		// the status (shown in the menu bar) current from here.
		go func() {
			for range time.Tick(statusEvery) {
				win.Update(func() {
					if !win.IsVisible() {
						a.pollStatus(time.Now())
					}
				})
			}
		}()
		toggle := func() {
			if win.IsVisible() && win.IsFocused() {
				hideWindow(win)
			} else {
				show()
			}
		}
		if *shortcut != "" {
			if err := mygo.GlobalShortcut.Register(*shortcut, toggle); err != nil {
				log.Printf("global shortcut %s: %v", *shortcut, err)
				a.notice = fmt.Sprintf("The shortcut %s is not available: %v", *shortcut, err)
				*shortcut = ""
			}
		}
		if err := menuBar(a, *shortcut, show); err != nil {
			log.Printf("menu-bar icon: %v", err)
			a.notice = "No menu-bar icon: " + err.Error()
		}
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}

// bundledSemd is the semd executable in the app bundle the UI runs from,
// or "" when the UI is not in a bundle (go run, tests).
func bundledSemd() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	macos := filepath.Dir(exe)
	contents := filepath.Dir(macos)
	if filepath.Base(macos) != "MacOS" || filepath.Base(contents) != "Contents" {
		return "", nil
	}
	semd := filepath.Join(contents, "Helpers", "semd")
	if _, err := os.Stat(semd); err != nil {
		return "", fmt.Errorf("damaged app bundle: %w", err)
	}
	return semd, nil
}

// ensureAgent makes launchd run semd at path, (re)installing the agent if
// it is missing, unloaded, or points at another binary.
func ensureAgent(label, semd, state string) error {
	// A quarantined app opened in place runs from a randomized read-only
	// copy (App Translocation), and one on a disk image goes away when it
	// is ejected: an agent pointing there would break.
	var fs syscall.Statfs_t
	if err := syscall.Statfs(semd, &fs); err != nil {
		return err
	}
	if fs.Flags&mntReadOnly != 0 || strings.Contains(semd, "/AppTranslocation/") {
		return fmt.Errorf("move Kim Quy to the Applications folder and open it from there (it runs from %s)", filepath.Dir(filepath.Dir(filepath.Dir(semd))))
	}
	spec, err := agent.Semd(label, semd, state, agent.BundleID)
	if err != nil {
		return err
	}
	m, err := agent.Default()
	if err != nil {
		return err
	}
	started, err := m.Ensure(spec)
	if err == nil && started {
		log.Printf("started semd as LaunchAgent %s (log: %s)", label, spec.LogPath)
	}
	return err
}

// agentService runs the bundled semd as a LaunchAgent.
type agentService struct {
	m                  *agent.Manager
	label, semd, state string
}

func (s *agentService) Label() string { return s.label }

func (s *agentService) LogPath() string { return agent.LogPath(home(), s.label) }

func (s *agentService) Info() (agent.Info, error) { return s.m.Info(s.label) }

func (s *agentService) Start() error { return ensureAgent(s.label, s.semd, s.state) }

func (s *agentService) Stop() error { return s.m.Stop(s.label) }

func (s *agentService) Restart() error { return s.m.Restart(s.label) }

func (s *agentService) Uninstall() ([]string, error) {
	code, out, err := runCommand("/usr/bin/getconf", "DARWIN_USER_CACHE_DIR")
	if err != nil || code != 0 {
		return nil, fmt.Errorf("getconf DARWIN_USER_CACHE_DIR: exit %d, %v: %s", code, err, strings.TrimSpace(out))
	}
	h := home()
	if h == "" {
		return nil, errors.New("no home directory")
	}
	u := &uninstaller{
		label:          s.label,
		bundleID:       agent.BundleID,
		state:          s.state,
		home:           h,
		app:            filepath.Dir(filepath.Dir(filepath.Dir(s.semd))), // X.app/Contents/Helpers/semd
		userCache:      filepath.Clean(strings.TrimSpace(out)),
		removeAgent:    s.m.Uninstall,
		setOpenAtLogin: mygo.App.SetOpenAtLogin,
		run:            runCommand,
		trash:          mygo.Shell.TrashItem,
		sweep:          func(paths []string) error { return sweepAfterExit(agent.BundleID, paths) },
	}
	return u.Uninstall()
}

// mntReadOnly is MNT_RDONLY of <sys/mount.h>.
const mntReadOnly = 0x1

// defaultShortcut shows or hides the window. Not Option-Space, which
// Raycast, Alfred and others take: macOS gives a shortcut to every app
// that registers it, so both would open.
const defaultShortcut = "Alt+Shift+Space"

// menuBar adds the icon to the menu bar. Its menu shows the state of the
// index, kept current by a.statusChanged.
func menuBar(a *app, shortcut string, show func()) error {
	icon, err := trayIcon()
	if err != nil {
		return err
	}
	tray, err := mygo.NewTray(mygo.TrayOptions{ID: "search", Icon: icon, IconIsTemplate: true, ToolTip: "Kim Quy"})
	if err != nil {
		return err
	}
	status := &mygo.MenuItem{Label: a.indexLine(), Disabled: true}
	pause := &mygo.MenuItem{Label: a.pauseLabel(), Disabled: a.statusErr != "",
		Click: func(*mygo.MenuItem, *mygo.Window) { a.togglePause() }}
	tray.SetMenu(mygo.NewMenu([]*mygo.MenuItem{
		{Label: "Search Files", Accelerator: shortcut, Click: func(*mygo.MenuItem, *mygo.Window) { show() }},
		{Label: "Settings…", Click: func(*mygo.MenuItem, *mygo.Window) {
			show()
			a.openSettings()
		}},
		{Label: "Activity…", Click: func(*mygo.MenuItem, *mygo.Window) {
			show()
			a.openSettings()
			a.settingsTab = tabActivity
		}},
		mygo.Separator(),
		status,
		pause,
		mygo.Separator(),
		{Label: "Open at Login", Type: mygo.MenuItemCheckbox, Checked: mygo.App.OpenAtLogin(),
			Click: func(it *mygo.MenuItem, _ *mygo.Window) {
				if err := mygo.App.SetOpenAtLogin(it.Checked); err != nil {
					log.Printf("open at login: %v", err)
					it.SetChecked(!it.Checked)
					a.notice = "Cannot change Open at Login: " + err.Error()
				}
			}},
		mygo.Separator(),
		{Role: mygo.RoleQuit, Label: "Quit Kim Quy"},
	}))
	a.statusChanged = func() {
		status.SetLabel(a.indexLine())
		pause.SetLabel(a.pauseLabel())
		pause.SetEnabled(a.statusErr == "")
	}
	mygo.App.OnWillQuit(func(*mygo.QuitEvent) { tray.Destroy() })
	return nil
}

// trayIcon draws a magnifying glass over a page, 16 points at 2x, black on
// transparent for macOS to tint.
func trayIcon() ([]byte, error) {
	const n = 32
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	ink := color.NRGBA{A: 255}
	for y := range n {
		for x := range n {
			// The page: an outline and a line of text.
			page := (x == 3 || x == 4 || x == 20 || x == 21) && y >= 2 && y <= 27 ||
				(y == 2 || y == 3 || y == 26 || y == 27) && x >= 3 && x <= 21 ||
				(y == 8 || y == 9) && x >= 7 && x <= 17
			dx, dy := float64(x)-19.5, float64(y)-19.5
			r := dx*dx + dy*dy
			lens := r <= 7.5*7.5
			ring := lens && r >= 5*5
			handle := x >= 24 && y >= 24 && x-y >= -2 && x-y <= 2 && x <= 30 && y <= 30
			switch {
			case ring || handle:
				img.Set(x, y, ink)
			case lens: // the lens covers the page
			case page:
				img.Set(x, y, ink)
			}
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

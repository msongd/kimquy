package main

import (
	"fmt"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/msongd/kimquy/internal/agent"
)

// service controls semd as a LaunchAgent. Its methods block: the UI calls
// them off the main thread. It is nil when the UI does not manage semd
// (not run from Kim Quy.app, or with -agent=false).
type service interface {
	Label() string
	LogPath() string
	Info() (agent.Info, error)
	// Start installs the agent if needed and has launchd run it.
	Start() error
	// Stop unloads the agent; it starts again at the next login.
	Stop() error
	Restart() error
	// Uninstall removes Kim Quy and all it made, and reports what it
	// removed, in order, even when it fails part way.
	Uninstall() (removed []string, err error)
}

const infoEvery = 2 * time.Second // while the Service tab shows

// pollInfo asks launchd about semd while the Service tab shows.
func (a *app) pollInfo(now time.Time) time.Time {
	if a.svc == nil || !a.settingsOpen || a.settingsTab != tabService || a.infoBusy {
		return time.Time{}
	}
	if now.Before(a.infoDue) {
		return a.infoDue
	}
	a.infoBusy, a.infoDue = true, now.Add(infoEvery)
	a.async(func() func() {
		in, err := a.svc.Info()
		return func() {
			a.infoBusy = false
			a.info, a.infoErr = in, ""
			if err != nil {
				a.info, a.infoErr = agent.Info{}, err.Error()
			}
			a.infoLoaded = true
		}
	})
	return time.Time{}
}

// serviceState says whether semd runs, in a few words.
func (a *app) serviceState() string {
	in := a.info
	switch {
	case a.infoErr != "":
		return "unknown: " + a.infoErr
	case !a.infoLoaded:
		return "…"
	case !in.Loaded:
		return "stopped"
	case in.PID > 0:
		return fmt.Sprintf("%s (pid %d)", in.State, in.PID)
	}
	return in.State
}

func (a *app) connectionState() string {
	switch {
	case a.statusErr != "":
		return "not connected"
	case a.live:
		return "connected, live events"
	}
	return "connected, polling"
}

func (a *app) serviceSection(c *ui.Context) {
	t := c.Theme()
	if a.svc == nil {
		ui.Text(c, "Kim Quy does not run semd here (it runs from Kim Quy.app only): control it with semctl and launchctl.").TextColor(t.TextMuted).Wrap()
		ui.Text(c, "Connection: "+a.connectionState()).FontSize(11).TextColor(t.TextMuted).SingleLine()
		return
	}
	in := a.info
	lines := []string{
		"semd: " + a.serviceState(),
		"Connection: " + a.connectionState(),
		"LaunchAgent: " + a.svc.Label(),
	}
	if in.Loaded {
		lines = append(lines,
			"Program: "+orDash(in.Program),
			fmt.Sprintf("Started %d times since loaded · last exit: %s", in.Runs, orDash(in.LastExit)))
	}
	lines = append(lines, "Log: "+tildePath(a.svc.LogPath()))
	for _, l := range lines {
		ui.Text(c, l).FontSize(12).SingleLine().Tooltip(l)
	}
	if a.acting != "" {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Spinner(c)
			ui.Text(c, a.acting).TextColor(t.TextMuted)
		})
	}
	ui.Column(c).Grow(1)
	busy := a.acting != "" || !a.infoLoaded
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		if in.Loaded {
			if ui.Button(c, "Stop semd").Tooltip("Until the next login, or Start").Disabled(busy).Clicked() {
				a.serviceAction("Stopping semd…", a.svc.Stop)
			}
		} else if ui.Button(c, "Start semd").Disabled(busy).Clicked() {
			a.serviceAction("Starting semd…", a.svc.Start)
		}
		if ui.Button(c, "Restart semd").Disabled(busy || !in.Loaded).Clicked() {
			a.serviceAction("Restarting semd…", a.svc.Restart)
		}
		if ui.Button(c, "Open Log").Clicked() {
			if err := a.sh.Open(a.svc.LogPath()); err != nil {
				a.notice = "Cannot open the log: " + err.Error()
			}
		}
		ui.Column(c).Grow(1)
		if ui.Button(c, "Uninstall Kim Quy…").TextColor(t.Danger).Disabled(a.acting != "").Clicked() {
			a.askUninstall = true
		}
	})
}

// serviceAction runs an action on the agent off the main thread, then
// looks at it again.
func (a *app) serviceAction(doing string, fn func() error) {
	if a.svc == nil || a.acting != "" {
		panic("serviceAction: no service, or an action in flight")
	}
	a.acting, a.notice = doing, ""
	a.async(func() func() {
		err := fn()
		return func() {
			a.acting = ""
			a.infoDue, a.statusDue = time.Time{}, time.Time{}
			if err != nil {
				a.notice = fmt.Sprintf("%s failed: %v", doing[:len(doing)-len("…")], err)
				return
			}
			a.agentErr = ""
		}
	})
}

// The uninstall flow: confirm, work (nothing else can be done meanwhile),
// then a last screen that only quits.

const uninstallText = "This stops semd and deletes the index, the model, the settings and the logs. " +
	"It removes Kim Quy from Login Items and its macOS permissions, and moves Kim Quy.app to the Trash. " +
	"Your files are not touched."

func (a *app) uninstallView(c *ui.Context) {
	switch ui.AlertDialog(c, &a.askUninstall, "Uninstall Kim Quy?", uninstallText, "Cancel", "Uninstall") {
	case 1:
		a.uninstall()
	}
	if !a.uninstalling && a.uninstalled == nil && a.uninstallErr == "" {
		return
	}
	open := true // it cannot be dismissed
	ui.Modal(c, &open, func() {
		t := c.Theme()
		ui.Column(c).Width(sheetW).Gap(10).Children(func() {
			switch {
			case a.uninstalling:
				ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
					ui.Spinner(c)
					ui.Text(c, "Uninstalling Kim Quy…").Bold()
				})
				return
			case a.uninstallErr != "":
				ui.Text(c, "Kim Quy is not completely uninstalled").FontSize(17).Bold()
				ui.Text(c, a.uninstallErr).TextColor(t.Danger).Wrap()
			default:
				ui.Text(c, "Kim Quy is uninstalled").FontSize(17).Bold()
			}
			if len(a.uninstalled) > 0 {
				ui.Text(c, "Removed:").TextColor(t.TextMuted)
				for _, r := range a.uninstalled {
					ui.Text(c, "• "+r).FontSize(11).SingleLine().Tooltip(r)
				}
			}
			if a.uninstallErr != "" {
				ui.Text(c, "Fix the problem and try again: what is left stays in place.").TextColor(t.TextMuted).Wrap()
				ui.Row(c).Gap(8).Children(func() {
					ui.Column(c).Grow(1)
					if ui.Button(c, "Close").Clicked() {
						a.uninstallErr, a.uninstalled = "", nil
					}
					if ui.Button(c, "Try Again").Clicked() {
						a.uninstall()
					}
				})
				return
			}
			ui.Text(c, "If you gave semd Full Disk Access, remove it from the list in System Settings › Privacy & Security › Full Disk Access: macOS does not let apps do that.").
				TextColor(t.TextMuted).Wrap()
			ui.Row(c).Gap(8).Children(func() {
				ui.Column(c).Grow(1)
				if ui.Button(c, "Open Privacy Settings").Clicked() {
					if err := a.sh.OpenPrivacySettings(); err != nil {
						a.notice = "Could not open System Settings: " + err.Error()
					}
				}
				if ui.Button(c, "Quit").Clicked() {
					a.sh.Quit()
				}
			})
		})
	})
}

func (a *app) uninstall() {
	if a.svc == nil || a.uninstalling {
		panic("uninstall: no service, or already uninstalling")
	}
	a.settingsOpen, a.askUninstall = false, false
	a.uninstalling, a.uninstallErr, a.uninstalled = true, "", nil
	a.async(func() func() {
		removed, err := a.svc.Uninstall()
		return func() {
			a.uninstalling = false
			a.uninstalled = append([]string{}, removed...) // not nil: done
			if err != nil {
				a.uninstallErr = err.Error()
			}
		}
	})
}

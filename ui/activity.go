package main

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/msongd/kimquy/internal/events"
)

// The activity log: semd's events as they come, newest first, kept in the
// UI's memory only.

const (
	maxActivity  = 500 // the oldest go first
	activityPage = 8
)

// activityQueue holds events from the stream's goroutine until the main
// thread takes them: a burst of thousands costs one redraw.
type activityQueue struct {
	mu    sync.Mutex
	items []events.Event
}

func (q *activityQueue) add(ev events.Event) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, ev)
	if n := len(q.items); n > 2*maxActivity { // only the last maxActivity are shown
		q.items = slices.Clone(q.items[n-maxActivity:])
	}
}

func (q *activityQueue) take() []events.Event {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.items
	q.items = nil
	return items
}

// addActivity appends events to the log, oldest first, dropping the oldest
// beyond maxActivity.
func (a *app) addActivity(evs []events.Event) {
	if len(evs) == 0 {
		return
	}
	a.activity = append(a.activity, evs...)
	if n := len(a.activity); n > maxActivity {
		a.activity = slices.Clone(a.activity[n-maxActivity:])
	}
}

// eventLabel names an event's type for people. semd may send types this
// UI does not know: they show as they are.
func eventLabel(typ string) string {
	switch typ {
	case "indexed":
		return "Indexed"
	case "removed":
		return "Removed"
	case "failed":
		return "Failed"
	case "pause":
		return "Pause"
	case "fatal":
		return "Fatal"
	case "scan":
		return "Scan"
	case "retry":
		return "Retry"
	case "slow":
		return "Slow"
	case "reindex":
		return "Rebuild"
	case "watch":
		return "Watch"
	case "lagged":
		return "Missed"
	case "connected":
		return "Connected"
	case "disconnected":
		return "Lost"
	}
	return typ
}

// bad reports whether an event is a problem, shown in the danger color.
func bad(typ string) bool {
	return typ == "failed" || typ == "fatal" || typ == "disconnected" || typ == "lagged"
}

func (a *app) activitySection(c *ui.Context) {
	t := c.Theme()
	shown := a.activity
	if a.activityFrozen != nil {
		shown = a.activityFrozen
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		var state string
		switch {
		case a.activityFrozen != nil:
			state = "Held: newer events are not shown"
		case a.live:
			state = "Live"
		default:
			state = "Not connected to semd's events"
		}
		ui.Text(c, state).FontSize(11).TextColor(t.TextMuted).Grow(1).SingleLine()
		if a.activityFrozen != nil && ui.Button(c, "Back to Live").Clicked() {
			a.activityPage, a.activityFrozen = 0, nil
			shown = a.activity
		}
		if ui.Button(c, "Clear").Disabled(len(a.activity) == 0).Clicked() {
			a.activity, a.activityPage, a.activityFrozen = nil, 0, nil
			shown = nil
		}
	})
	if len(shown) == 0 {
		ui.Text(c, "Nothing yet: semd's work shows here as it happens.").TextColor(t.TextMuted)
	}
	lo, hi := page(&a.activityPage, len(shown), activityPage)
	for i := lo; i < hi; i++ {
		ev := shown[len(shown)-1-i] // newest first
		color := t.TextMuted
		if bad(ev.Type) {
			color = t.Danger
		}
		what := tildePath(ev.Path)
		switch {
		case what == "":
			what = ev.Msg
		case ev.Msg != "":
			what += " — " + ev.Msg
		}
		ui.Row(c).Key(fmt.Sprintf("%d", len(shown)-1-i)).Gap(10).AlignItems(ui.Center).Children(func() {
			ui.Text(c, ev.Time.Local().Format(time.TimeOnly)).FontSize(11).TextColor(t.TextMuted)
			ui.Text(c, eventLabel(ev.Type)).FontSize(11).Bold().TextColor(color).Width(64).SingleLine()
			ui.Text(c, what).FontSize(12).SingleLine().Grow(1).Tooltip(what)
		})
	}
	ui.Column(c).Grow(1)
	ui.Row(c).AlignItems(ui.Center).Children(func() {
		ui.Text(c, fmt.Sprintf("Last %d events, kept while Kim Quy runs.", maxActivity)).FontSize(11).TextColor(t.TextMuted).Grow(1)
		pager(c, &a.activityPage, len(shown), activityPage, "activity")
	})
	// Off the first page, hold the log as it is: new events would shift
	// the page being read.
	if a.activityPage > 0 && a.activityFrozen == nil {
		a.activityFrozen = slices.Clone(a.activity)
	}
}

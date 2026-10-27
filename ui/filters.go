package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/msongd/kimquy/internal/client"
)

// The search options under the search field: which kinds of file, which
// folder, and how many results. A change searches again at once.

// kindChoice is a kind semd indexes (kind.Kind's name) and its menu label.
type kindChoice struct{ name, label string }

var kindChoices = []kindChoice{
	{"text", "Text"}, {"office", "Office"}, {"pdf", "PDF"},
	{"image", "Images"}, {"audio", "Audio"}, {"video", "Video"},
}

var limitChoices = []int{10, 25, 50, 100} // semd allows 1–100

const defaultLimit = 50

func (a *app) filtersRow(c *ui.Context) {
	ui.Row(c).Padding(0, 16, 10, 16).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.MenuButton(c, a.kindsLabel(), func(m *ui.Menu) {
			if m.Item("All Kinds").Checked(len(a.kinds) == 0).Chosen() {
				a.setKinds(nil)
			}
			m.Separator()
			for _, k := range kindChoices {
				on := slices.Contains(a.kinds, k.name)
				if m.Item(k.label).Checked(on).Chosen() {
					a.toggleKind(k.name, !on)
				}
			}
		}).Label("Kinds")
		roots := a.rootPaths()
		ui.MenuButton(c, a.rootLabel(), func(m *ui.Menu) {
			if m.Item("All Folders").Checked(a.root == "").Chosen() {
				a.setRoot("")
			}
			if len(roots) > 0 {
				m.Separator()
			}
			for _, r := range roots {
				if m.Item(tildePath(r)).Checked(a.root == r).Chosen() {
					a.setRoot(r)
				}
			}
		}).Label("Folder").Tooltip(orDash(tildePath(a.root)))
		ui.MenuButton(c, fmt.Sprintf("%d Results", a.limit), func(m *ui.Menu) {
			for _, n := range limitChoices {
				if m.Item(fmt.Sprintf("%d Results", n)).Checked(a.limit == n).Chosen() {
					a.setLimit(n)
				}
			}
		}).Label("Number of results")
		if a.filtered() && ui.Button(c, "Reset").Tooltip("All kinds, all folders").Clicked() {
			a.kinds, a.root = nil, ""
			a.filtersChanged()
		}
	})
}

func (a *app) kindsLabel() string {
	switch len(a.kinds) {
	case 0:
		return "All Kinds"
	case 1, 2:
		labels := make([]string, len(a.kinds))
		for i, k := range a.kinds {
			labels[i] = kindChoiceLabel(k)
		}
		return strings.Join(labels, ", ")
	}
	return fmt.Sprintf("%d Kinds", len(a.kinds))
}

func kindChoiceLabel(name string) string {
	for _, k := range kindChoices {
		if k.name == name {
			return k.label
		}
	}
	panic(fmt.Sprintf("kindChoiceLabel: unknown kind %q", name))
}

func (a *app) rootLabel() string {
	if a.root == "" {
		return "All Folders"
	}
	return filepath.Base(a.root)
}

// filtered reports whether the results are narrowed by kind or folder.
func (a *app) filtered() bool { return len(a.kinds) > 0 || a.root != "" }

// rootPaths are the folders semd indexes, as its status last said.
func (a *app) rootPaths() []string {
	out := make([]string, 0, len(a.status.Roots))
	for _, r := range a.status.Roots {
		out = append(out, r.Path)
	}
	return out
}

func (a *app) setKinds(kinds []string) {
	a.kinds = kinds
	a.filtersChanged()
}

// toggleKind adds or removes a kind, keeping the menu's order. Every kind
// chosen is all kinds.
func (a *app) toggleKind(name string, on bool) {
	var kinds []string
	for _, k := range kindChoices {
		if k.name == name && on || k.name != name && slices.Contains(a.kinds, k.name) {
			kinds = append(kinds, k.name)
		}
	}
	if len(kinds) == len(kindChoices) {
		kinds = nil
	}
	a.setKinds(kinds)
}

func (a *app) setRoot(r string) {
	if r != "" && !slices.Contains(a.rootPaths(), r) {
		panic(fmt.Sprintf("setRoot: %q is not indexed", r))
	}
	a.root = r
	a.filtersChanged()
}

func (a *app) setLimit(n int) {
	if !slices.Contains(limitChoices, n) {
		panic(fmt.Sprintf("setLimit: %d is not a choice", n))
	}
	a.limit = n
	a.filtersChanged()
}

// rootsUpdated drops the folder filter when semd no longer indexes it.
func (a *app) rootsUpdated() {
	if a.root != "" && !slices.Contains(a.rootPaths(), a.root) {
		a.root = ""
		a.filtersChanged()
	}
}

// filtersChanged searches again now, without the typing pause.
func (a *app) filtersChanged() {
	if q := strings.TrimSpace(a.query); q != "" {
		a.due = time.Time{}
		a.search(q)
	}
}

// searchRequest is the search for q with the options chosen.
func (a *app) searchRequest(q string) client.SearchRequest {
	r := client.SearchRequest{Q: q, K: a.limit, Kinds: slices.Clone(a.kinds)}
	if a.root != "" {
		r.Roots = []string{a.root}
	}
	return r
}

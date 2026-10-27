package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/msongd/kimquy/internal/model"
)

// models is the embedding model semd runs, as the app installs it. Dir
// and Bundled answer at once; the others block, so the UI calls them off
// the main thread.
type models interface {
	// Dir is the model directory semd uses.
	Dir() string
	// Bundled reports whether it is in the app bundle, which the app does
	// not change.
	Bundled() bool
	// Installed reports which parts Dir holds, by folder name.
	Installed() (map[string]bool, error)
	// Free is the space left on Dir's volume.
	Free() (int64, error)
	Download(context.Context, []model.Part, model.Progress) error
	// Import copies the parts a folder or app has, and reports which.
	Import(context.Context, string, model.Progress) ([]model.Part, error)
}

// stateModels installs the model into semd's state directory.
type stateModels struct {
	semd, state string // semd in the app bundle, and its state directory
	in          *model.Installer
}

func newStateModels(semd, state string) *stateModels {
	return &stateModels{semd: semd, state: state, in: model.NewInstaller(model.Dir(state))}
}

func (m *stateModels) Dir() string   { return model.Find(m.semd, m.state) }
func (m *stateModels) Bundled() bool { return model.Bundled(m.semd) != "" }

func (m *stateModels) Installed() (map[string]bool, error) { return model.Installed(m.Dir()) }

func (m *stateModels) Free() (int64, error) {
	// The directory may not exist yet: its parent's volume is the one.
	for dir := m.in.Dir; ; {
		var st syscall.Statfs_t
		err := syscall.Statfs(dir, &st)
		if err == nil {
			return int64(st.Bavail) * int64(st.Bsize), nil
		}
		parent := filepath.Dir(dir)
		if !errors.Is(err, syscall.ENOENT) || parent == dir {
			return 0, err
		}
		dir = parent
	}
}

func (m *stateModels) Download(ctx context.Context, parts []model.Part, p model.Progress) error {
	if m.Bundled() {
		return errors.New("the model is built into the app")
	}
	return m.in.Download(ctx, parts, p)
}

func (m *stateModels) Import(ctx context.Context, src string, p model.Progress) ([]model.Part, error) {
	if m.Bundled() {
		return nil, errors.New("the model is built into the app")
	}
	return m.in.Import(ctx, src, p)
}

// modelState is what the app knows of the model and its installation.
type modelState struct {
	have    map[string]bool // nil until loaded
	haveErr string
	free    int64 // -1 when unknown
	busy    bool  // loading have and free

	want map[string]bool // the parts chosen on the setup screen

	// installing says what is under way ("Downloading", "Copying"), and
	// done and total how far it is; they are written off the main thread.
	installing  string
	done, total atomic.Int64
	cancel      context.CancelFunc
	installErr  string
	askAdd      model.Part // to confirm adding; Dir "" when none
	needRebuild bool       // a part was added to a model in use
}

// needsModel reports whether the app must install the model before semd
// can run: the setup screen shows instead of search.
func (a *app) needsModel() bool {
	return a.mdl != nil && a.model.have != nil && !a.model.have[model.Parts[0].Dir]
}

// loadModel looks at the model directory off the main thread, then starts
// semd if it has the model now and did not before.
func (a *app) loadModel() {
	if a.mdl == nil || a.model.busy {
		return
	}
	a.model.busy = true
	a.async(func() func() {
		have, err := a.mdl.Installed()
		free, ferr := a.mdl.Free()
		if ferr != nil {
			free = -1
		}
		return func() {
			a.model.busy = false
			a.model.have, a.model.haveErr = have, ""
			if a.model.have == nil {
				a.model.have = map[string]bool{}
			}
			if err != nil {
				a.model.haveErr = err.Error()
			}
			a.model.free = free
		}
	})
}

// install downloads parts, or imports from src when it is not "", off the
// main thread, then starts or restarts semd.
func (a *app) install(parts []model.Part, src string) {
	if a.mdl == nil || a.model.installing != "" {
		panic("install: no model store, or an installation in flight")
	}
	first := a.needsModel() // semd has not run: start it, nothing to rebuild
	ctx, cancel := context.WithCancel(context.Background())
	ms := &a.model
	ms.cancel, ms.installErr = cancel, ""
	ms.done.Store(0)
	ms.total.Store(0)
	ms.installing = "Downloading"
	if src != "" {
		ms.installing = "Copying"
	}
	progress := func(done, total int64) {
		ms.done.Store(done)
		ms.total.Store(total)
	}
	a.async(func() func() {
		defer cancel()
		added := parts
		var err error
		if src == "" {
			err = a.mdl.Download(ctx, parts, progress)
		} else {
			added, err = a.mdl.Import(ctx, src, progress)
		}
		return func() {
			ms.installing, ms.cancel = "", nil
			switch {
			case errors.Is(err, context.Canceled):
				ms.installErr = "Stopped. What was downloaded is kept: Download again to go on."
			case err != nil:
				ms.installErr = err.Error()
			}
			a.loadModel()
			if err != nil {
				return
			}
			if len(added) == 0 {
				ms.installErr = "Nothing to add: the model already has those parts."
				return
			}
			if a.svc == nil {
				a.notice = "The model is installed: restart semd to use it."
				return
			}
			if first {
				a.serviceAction("Starting semd…", a.svc.Start)
				return
			}
			// A new part makes a new model, whose vectors the index does not
			// hold: semd stops indexing until it is rebuilt.
			ms.needRebuild = true
			a.serviceAction("Restarting semd…", a.svc.Restart)
		}
	})
}

// chooseModel asks for a folder or app holding the model, then imports it.
func (a *app) chooseModel() {
	if a.model.installing != "" {
		return
	}
	a.model.installing, a.model.installErr = "Choosing", ""
	a.async(func() func() {
		src, err := a.sh.PickModel()
		return func() {
			a.model.installing = ""
			switch {
			case err != nil:
				a.model.installErr = "Cannot choose the model: " + err.Error()
			case src != "":
				a.install(nil, src)
			}
		}
	})
}

// chosenParts are the parts ticked on the setup screen, text first.
func (a *app) chosenParts() []model.Part {
	var out []model.Part
	for _, p := range model.Parts {
		if p.Required || a.model.want[p.Dir] {
			out = append(out, p)
		}
	}
	return out
}

func partsSize(parts []model.Part) int64 {
	var n int64
	for _, p := range parts {
		n += p.Size()
	}
	return n
}

// progressLine says how far the installation is.
func (a *app) progressLine() string {
	ms := &a.model
	done, total := ms.done.Load(), ms.total.Load()
	if ms.installing == "Choosing" {
		return "Choose the model…"
	}
	if total <= 0 {
		return ms.installing + "…"
	}
	return fmt.Sprintf("%s… %s of %s", ms.installing, model.Bytes(done), model.Bytes(total))
}

// installProgress shows the bar and Stop while installing, redrawing as it
// moves.
func (a *app) installProgress(c *ui.Context) {
	ms := &a.model
	c.After(250 * time.Millisecond)
	if total := ms.total.Load(); total > 0 {
		ui.Progress(c, min(1, float64(ms.done.Load())/float64(total)))
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Spinner(c)
		ui.Text(c, a.progressLine()).TextColor(c.Theme().TextMuted).Grow(1).SingleLine()
		if ms.cancel != nil && ui.Button(c, "Stop").Clicked() {
			ms.cancel()
		}
	})
}

func (a *app) freeLine(need int64) string {
	s := "Total: " + model.Bytes(need)
	if a.model.free >= 0 {
		s += " · " + model.Bytes(a.model.free) + " free on this disk"
	}
	return s
}

// setupView replaces the search window until semd has its model.
func (a *app) setupView(c *ui.Context) {
	t := c.Theme()
	ms := &a.model
	if ms.want == nil {
		ms.want = map[string]bool{"vision-q8": true}
	}
	ui.Column(c).Fill().Padding(28, 32).Gap(12).Children(func() {
		ui.Text(c, "Welcome to Kim Quy").FontSize(22).Bold()
		ui.Text(c, "Kim Quy searches your files by meaning with EmbeddingGemma 2, a model that runs on this Mac: "+
			"nothing you index leaves it. Download the model once, or use a copy you have (a folder holding "+
			model.Parts[0].Dir+", or an older Kim Quy.app).").Wrap()
		if ms.haveErr != "" {
			ui.Text(c, "The model is damaged: "+ms.haveErr+". Download it again.").TextColor(t.Danger).Wrap()
		}
		busy := ms.installing != ""
		for _, p := range model.Parts {
			ui.Row(c).Key(p.Dir).Gap(8).AlignItems(ui.Center).Children(func() {
				on := p.Required || ms.want[p.Dir]
				label := fmt.Sprintf("%s — %s", p.Label, model.Bytes(p.Size()))
				if p.Required {
					label += " (required)"
				}
				if ui.Checkbox(c, &on, label).Disabled(busy || p.Required).Changed() {
					ms.want[p.Dir] = on
				}
			})
		}
		parts := a.chosenParts()
		ui.Text(c, a.freeLine(partsSize(parts))).FontSize(12).TextColor(t.TextMuted)
		ui.Text(c, "Parts can be added later in Settings › Model; adding one rebuilds the index. "+
			"The model is from Hugging Face ("+model.Repo+"), under the "+model.License+" license.").
			FontSize(11).TextColor(t.TextMuted).Wrap()
		if ms.installErr != "" {
			ui.Text(c, ms.installErr).TextColor(t.Danger).Wrap()
		}
		if a.notice != "" {
			ui.Text(c, a.notice).TextColor(t.Danger).Wrap()
		}
		if busy {
			a.installProgress(c)
		}
		ui.Column(c).Grow(1)
		ui.Row(c).Gap(8).Children(func() {
			ui.Column(c).Grow(1)
			if ui.Button(c, "Use Files I Have…").Disabled(busy).Clicked() {
				a.chooseModel()
			}
			if ui.PrimaryButton(c, "Download").Disabled(busy || ms.have == nil).Clicked() {
				a.install(parts, "")
			}
		})
	})
}

// modelSection is the Model tab of the settings.
func (a *app) modelSection(c *ui.Context) {
	t := c.Theme()
	ms := &a.model
	if id := a.status.Embedder.Model; id != "" {
		ui.Text(c, "In use: "+id).FontSize(11).TextColor(t.TextMuted).SingleLine().Tooltip(id)
	}
	if a.mdl == nil {
		ui.Text(c, "Kim Quy does not manage the model here (it does from Kim Quy.app only): semd uses the one given by its -model flag.").
			TextColor(t.TextMuted).Wrap()
		return
	}
	where := tildePath(a.mdl.Dir())
	if a.mdl.Bundled() {
		where = "built into Kim Quy.app"
	}
	ui.Text(c, "Location: "+where).FontSize(12).SingleLine().Tooltip(a.mdl.Dir())
	if ms.haveErr != "" {
		ui.Text(c, ms.haveErr).FontSize(11).TextColor(t.Danger).SingleLine().Tooltip(ms.haveErr)
	}
	busy := ms.installing != "" || a.acting != ""
	for _, p := range model.Parts {
		ui.Row(c).Key(p.Dir).Gap(8).AlignItems(ui.Center).Children(func() {
			state := "not installed"
			if ms.have[p.Dir] {
				state = "installed"
			}
			ui.Text(c, fmt.Sprintf("%s — %s, %s", p.Label, model.Bytes(p.Size()), state)).Grow(1).SingleLine()
			if !ms.have[p.Dir] && !a.mdl.Bundled() && ms.have != nil {
				if ui.Button(c, "Add…").Label("Add " + p.Label).Disabled(busy).Clicked() {
					ms.askAdd = p
				}
			}
		})
	}
	if ms.installErr != "" {
		ui.Text(c, ms.installErr).FontSize(11).TextColor(t.Danger).SingleLine().Tooltip(ms.installErr)
	}
	if ms.installing != "" {
		a.installProgress(c)
	}
	if ms.needRebuild {
		ui.Text(c, "The model changed: rebuild the index to search with it (Indexing › Rebuild Index…).").
			FontSize(11).TextColor(t.Danger).Wrap()
	}
	ui.Column(c).Grow(1)
	ui.Row(c).Gap(8).Children(func() {
		if ui.Button(c, "Show in Finder").Clicked() {
			if err := a.sh.Reveal(a.mdl.Dir()); err != nil {
				a.notice = "Cannot show the model: " + err.Error()
			}
		}
		if !a.mdl.Bundled() && ui.Button(c, "Use Files I Have…").Disabled(busy).Clicked() {
			a.chooseModel()
		}
	})
}

// addPartView confirms adding a part from the Model tab.
func (a *app) addPartView(c *ui.Context) {
	ms := &a.model
	open := ms.askAdd.Dir != ""
	if !open {
		return
	}
	p := ms.askAdd
	msg := fmt.Sprintf("Kim Quy downloads %s, restarts semd, and then the index must be rebuilt: every file is read and embedded again.",
		model.Bytes(p.Size()))
	switch ui.AlertDialog(c, &open, "Add "+p.Label+"?", msg, "Cancel", "Download") {
	case 1:
		ms.askAdd = model.Part{}
		a.install([]model.Part{p}, "")
		return
	}
	if !open {
		ms.askAdd = model.Part{}
	}
}

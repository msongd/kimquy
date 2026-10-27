package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/msongd/kimquy/internal/agent"
	"github.com/msongd/kimquy/internal/model"
)

// fakeModels installs nothing: it records what it is asked, and Download
// blocks until the test sends its result on step (after one progress
// report) when step is set.
type fakeModels struct {
	dir       string
	bundled   bool
	have      map[string]bool
	haveErr   error
	free      int64
	fail      error
	downloads [][]string
	imports   []string
	step      chan error
}

func (m *fakeModels) Dir() string   { return m.dir }
func (m *fakeModels) Bundled() bool { return m.bundled }
func (m *fakeModels) Installed() (map[string]bool, error) {
	return maps(m.have), m.haveErr
}
func (m *fakeModels) Free() (int64, error) { return m.free, nil }
func (m *fakeModels) Download(ctx context.Context, parts []model.Part, p model.Progress) error {
	var dirs []string
	for _, pt := range parts {
		dirs = append(dirs, pt.Dir)
	}
	m.downloads = append(m.downloads, dirs)
	if m.step != nil {
		p(100e6, partsSize(parts))
		select {
		case err := <-m.step:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if m.fail != nil {
		return m.fail
	}
	for _, d := range dirs {
		m.have[d] = true
	}
	return nil
}
func (m *fakeModels) Import(_ context.Context, src string, _ model.Progress) ([]model.Part, error) {
	m.imports = append(m.imports, src)
	if m.fail != nil {
		return nil, m.fail
	}
	var added []model.Part
	for _, p := range model.Parts[:2] {
		if !m.have[p.Dir] {
			m.have[p.Dir] = true
			added = append(added, p)
		}
	}
	return added, nil
}

func maps(m map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func newModelHarness(t *testing.T, have map[string]bool) (*harness, *fakeModels, *fakeService) {
	h := &harness{t: t, semd: &fakeSemd{}}
	h.start(h.semd)
	m := &fakeModels{dir: "/Users/me/Library/Application Support/Kim Quy/models", have: have, free: 50e9}
	svc := &fakeService{}
	h.a.mdl, h.a.svc = m, svc
	h.Frame()
	h.flush()
	return h, m, svc
}

func TestSetupDownload(t *testing.T) {
	h, m, svc := newModelHarness(t, map[string]bool{})
	h.want("Welcome to Kim Quy")
	if h.HasText("Type to search the contents of your files.") {
		t.Fatal("search shows without a model")
	}
	h.want("Text and documents — 320 MB (required)")
	h.want("Total: 513 MB · 50.0 GB free on this disk")
	h.click("Audio and the sound of videos — 611 MB")
	h.want("Total: 1.1 GB · 50.0 GB free on this disk")
	h.click("Images, PDFs and video frames — 193 MB")
	h.want("Total: 932 MB · 50.0 GB free on this disk")
	h.click("Text and documents — 320 MB (required)") // disabled
	h.want("Total: 932 MB · 50.0 GB free on this disk")

	m.step = make(chan error)
	h.click("Download")
	// The download runs (off the main thread in the app): let it report,
	// then hold it there to look at the screen.
	w := h.queue[0]
	h.queue = h.queue[1:]
	done := make(chan func())
	go func() { done <- w() }()
	for h.a.model.done.Load() == 0 {
	}
	h.Frame()
	h.want("Downloading… 100 MB of 932 MB")
	if svc.starts != 0 {
		t.Fatal("semd started before the model is in")
	}
	m.step <- nil
	(<-done)()
	h.flush()
	if !slices.Equal(m.downloads[0], []string{"text-q8", "audio-bf16"}) {
		t.Fatalf("downloaded %v", m.downloads)
	}
	if svc.starts != 1 || svc.restarts != 0 {
		t.Fatalf("starts %d, restarts %d", svc.starts, svc.restarts)
	}
	h.want("Type to search the contents of your files.")
	if h.a.model.needRebuild {
		t.Error("a first install asks for a rebuild")
	}
}

func TestSetupStopAndFail(t *testing.T) {
	h, m, svc := newModelHarness(t, map[string]bool{})
	m.step = make(chan error)
	h.click("Download")
	w := h.queue[0]
	h.queue = h.queue[1:]
	done := make(chan func())
	go func() { done <- w() }()
	for h.a.model.done.Load() == 0 {
	}
	h.Frame()
	h.click("Stop")
	(<-done)()
	h.flush()
	h.want("Stopped. What was downloaded is kept: Download again to go on.")

	m.step, m.fail = nil, errors.New("model: 1.0 GB free on this disk; the model needs 513 MB")
	h.click("Download")
	h.flush()
	h.want("model: 1.0 GB free on this disk; the model needs 513 MB")
	h.want("Welcome to Kim Quy")
	if svc.starts != 0 {
		t.Fatal("semd started without a model")
	}
}

func TestSetupImport(t *testing.T) {
	h, m, svc := newModelHarness(t, map[string]bool{})
	h.shell.pickModel = "/Applications/Old Kim Quy.app"
	h.click("Use Files I Have…")
	h.flush()
	if !slices.Equal(m.imports, []string{"/Applications/Old Kim Quy.app"}) {
		t.Fatalf("imports %v", m.imports)
	}
	if svc.starts != 1 {
		t.Fatalf("%d starts", svc.starts)
	}
	h.want("Type to search the contents of your files.")
}

func TestSetupDamaged(t *testing.T) {
	h, _, _ := newModelHarness(t, map[string]bool{})
	h.a.mdl.(*fakeModels).haveErr = errors.New("model: text-q8 is damaged")
	h.a.loadModel()
	h.flush()
	h.want("The model is damaged: model: text-q8 is damaged. Download it again.")
}

func TestModelTab(t *testing.T) {
	h, m, svc := newModelHarness(t, map[string]bool{"text-q8": true})
	svc.info = agent.Info{Loaded: true, State: "running"}
	h.click("Settings")
	h.flush()
	h.click("Model")
	h.want("Location: ~/Library/Application Support/Kim Quy/models")
	h.want("Text and documents — 320 MB, installed")
	h.want("Images, PDFs and video frames — 193 MB, not installed")
	h.click("Show in Finder")
	if !slices.Equal(h.shell.revealed, []string{m.dir}) {
		t.Fatalf("revealed %v", h.shell.revealed)
	}

	h.click("Add Images, PDFs and video frames")
	h.want("Add Images, PDFs and video frames?")
	h.click("Cancel")
	h.flush()
	if len(m.downloads) != 0 || h.a.model.askAdd.Dir != "" {
		t.Fatal("downloaded after Cancel")
	}
	h.click("Add Images, PDFs and video frames")
	h.click("Download")
	h.flush()
	h.flush()
	if !slices.Equal(m.downloads[0], []string{"vision-q8"}) {
		t.Fatalf("downloaded %v", m.downloads)
	}
	if svc.restarts != 1 || svc.starts != 0 {
		t.Fatalf("restarts %d, starts %d", svc.restarts, svc.starts)
	}
	h.want("Images, PDFs and video frames — 193 MB, installed")
	h.want("The model changed: rebuild the index to search with it (Indexing › Rebuild Index…).")
	h.a.reindex()
	h.flush()
	if h.a.model.needRebuild {
		t.Error("still asks for a rebuild")
	}

	// Importing what is there already says so.
	h.shell.pickModel = "/Volumes/Backup/models"
	h.click("Use Files I Have…")
	h.flush()
	h.want("Nothing to add: the model already has those parts.")
}

func TestModelTabBundled(t *testing.T) {
	h, _, _ := newModelHarness(t, map[string]bool{"text-q8": true, "vision-q8": true, "audio-bf16": true})
	h.a.mdl.(*fakeModels).bundled = true
	h.click("Settings")
	h.flush()
	h.click("Model")
	h.want("Location: built into Kim Quy.app")
	if h.HasText("Use Files I Have…") {
		t.Fatal("offers to change a bundled model")
	}

	// Without a model store (not from the app) the tab only informs.
	h2 := newHarness(t)
	h2.click("Settings")
	h2.flush()
	h2.click("Model")
	h2.want("Kim Quy does not manage the model here (it does from Kim Quy.app only): semd uses the one given by its -model flag.")
}

func TestStateModels(t *testing.T) {
	root := t.TempDir()
	semd := filepath.Join(root, "Kim Quy.app", "Contents", "Helpers", "semd")
	state := filepath.Join(root, "state")
	m := newStateModels(semd, state)
	if m.Bundled() || m.Dir() != filepath.Join(state, "models") {
		t.Fatalf("slim app: bundled %v, dir %s", m.Bundled(), m.Dir())
	}
	if free, err := m.Free(); err != nil || free <= 0 { // state does not exist yet
		t.Fatalf("free %d, %v", free, err)
	}
	have, err := m.Installed()
	if err != nil || have["text-q8"] {
		t.Fatalf("have %v, %v", have, err)
	}
	bundled := filepath.Join(root, "Kim Quy.app", "Contents", "Resources", "models", "text-q8")
	if err := os.MkdirAll(bundled, 0o755); err != nil {
		t.Fatal(err)
	}
	if !m.Bundled() || !strings.HasSuffix(m.Dir(), "Resources/models") {
		t.Fatalf("bundled app: %v %s", m.Bundled(), m.Dir())
	}
	if err := m.Download(context.Background(), model.Parts[:1], nil); err == nil {
		t.Fatal("downloaded into a bundled app")
	}
}

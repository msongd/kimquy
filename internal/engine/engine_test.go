package engine

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msongd/kimquy/internal/embed"
	"github.com/msongd/kimquy/internal/events"
	"github.com/msongd/kimquy/internal/kind"
)

type harness struct {
	t      *testing.T
	state  string
	root   string
	e      *Engine
	sub    *events.Sub
	cancel context.CancelFunc
	done   chan error
	wait   time.Duration // idle() deadline; default 10s
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func start(t *testing.T, state, root string, emb embed.Embedder) *harness {
	t.Helper()
	bus := events.NewBus()
	e, err := Open(Options{StateDir: state, Embedder: emb, Bus: bus})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, state: state, root: root, e: e, sub: bus.Subscribe(4096), done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- e.Run(ctx) }()
	return h
}

func (h *harness) stop() {
	h.t.Helper()
	h.cancel()
	if err := <-h.done; err != nil {
		h.t.Fatalf("Run: %v", err)
	}
	if err := h.e.Close(); err != nil {
		h.t.Fatalf("Close: %v", err)
	}
}

// idle waits until no scan is requested or running and the queue is empty,
// stable across several polls.
func (h *harness) idle() {
	h.t.Helper()
	wait := h.wait
	if wait == 0 {
		wait = 10 * time.Second
	}
	deadline := time.Now().Add(wait)
	for quiet := 0; quiet < 5; {
		select {
		case ev := <-h.sub.C:
			if ev.Type == "fatal" {
				h.t.Fatalf("fatal: %s", ev.Msg)
			}
			continue
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for idle: %+v", h.e.Status())
		}
		if len(h.e.scanReq) == 0 && h.e.scanning.Load() == 0 && h.e.scanReqN.Load() == h.e.scanDoneN.Load() && h.e.q.Len() == 0 {
			quiet++
		} else {
			quiet = 0
		}
	}
}

func (h *harness) search(q string, k int) []Hit {
	h.t.Helper()
	hits, err := h.e.Search(context.Background(), SearchRequest{Q: q, K: k})
	if err != nil {
		h.t.Fatalf("search %q: %v", q, err)
	}
	return hits
}

func docx(t *testing.T, path, text string) {
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("word/document.xml")
	w.Write([]byte(`<w:document xmlns:w="w"><w:body><w:p><w:r><w:t>` + text + `</w:t></w:r></w:p></w:body></w:document>`))
	zw.Close()
	f.Close()
}

func TestEndToEnd(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	write(t, filepath.Join(root, "notes/zebra.md"), "Zebras have black and white stripes and live in Africa.")
	write(t, filepath.Join(root, "notes/rocket.txt"), "The rocket engine burns liquid oxygen and kerosene during launch.")
	write(t, filepath.Join(root, "src/main.go"), "package main\n\n// compute the fibonacci sequence recursively\nfunc fib(n int) int { return n }\n")
	write(t, filepath.Join(root, "README"), "Penguins waddle across antarctic ice sheets.")
	write(t, filepath.Join(root, "node_modules/x/index.js"), "zebra stripes zebra stripes")
	write(t, filepath.Join(root, ".hidden/z.txt"), "zebra stripes")
	write(t, filepath.Join(root, "bad.txt"), "zebra \xff\xfe invalid")
	write(t, filepath.Join(root, "blob"), "\x00\x01binary")
	write(t, filepath.Join(root, "big.txt"), strings.Repeat("big ", 300))
	docx(t, filepath.Join(root, "report.docx"), "Quarterly revenue grew because of strong umbrella sales")

	h := start(t, state, root, &embed.Fake{Dims: 768})
	if _, err := h.e.SetRoots([]string{root}, []string{"big.txt"}); err != nil {
		t.Fatal(err)
	}
	h.idle()

	st := h.e.Status()
	if st.Indexed != 5 || st.Failed != 1 {
		t.Fatalf("status %+v, failures %+v", st, h.e.Failures())
	}
	if f := h.e.Failures(); len(f) != 1 || !strings.HasSuffix(f[0].Path, "bad.txt") || !strings.Contains(f[0].Error, "UTF-8") {
		t.Fatalf("failures %+v", f)
	}
	for q, want := range map[string]string{
		"zebra stripes":        "notes/zebra.md",
		"rocket launch oxygen": "notes/rocket.txt",
		"fibonacci sequence":   "src/main.go",
		"penguins antarctic":   "README",
		"umbrella revenue":     "report.docx",
	} {
		hits := h.search(q, 3)
		if len(hits) == 0 || hits[0].Path != filepath.Join(root, want) {
			t.Fatalf("%q: top hit %+v, want %s", q, hits, want)
		}
		if hits[0].Snippet == "" || hits[0].Stale || hits[0].Locator.Kind != kind.LocBytes {
			t.Fatalf("%q: bad hit details %+v", q, hits[0])
		}
	}
	for _, hit := range h.search("zebra stripes", 10) {
		if strings.Contains(hit.Path, "node_modules") || strings.Contains(hit.Path, ".hidden") {
			t.Fatalf("excluded file indexed: %s", hit.Path)
		}
	}
	// Root and kind filters.
	hits, err := h.e.Search(context.Background(), SearchRequest{Q: "zebra", K: 10, Roots: []string{filepath.Join(root, "src")}})
	if err != nil || len(hits) != 1 || !strings.HasSuffix(hits[0].Path, "main.go") {
		t.Fatalf("root filter: %+v %v", hits, err)
	}
	hits, _ = h.e.Search(context.Background(), SearchRequest{Q: "zebra", K: 10, Kinds: kind.SetOf(kind.Office)})
	if len(hits) != 1 || !strings.HasSuffix(hits[0].Path, "report.docx") {
		t.Fatalf("kind filter: %+v", hits)
	}

	// Edit, delete, and rescan.
	write(t, filepath.Join(root, "notes/zebra.md"), "Giraffes have very long necks.")
	os.Remove(filepath.Join(root, "notes/rocket.txt"))
	h.e.RequestScan()
	h.idle()
	if hits := h.search("giraffes necks", 1); hits[0].Path != filepath.Join(root, "notes/zebra.md") {
		t.Fatalf("edit not picked up: %+v", hits)
	}
	for _, hit := range h.search("rocket launch oxygen", 10) {
		if strings.HasSuffix(hit.Path, "rocket.txt") {
			t.Fatal("deleted file still found")
		}
	}
	if st := h.e.Status(); st.Indexed != 4 {
		t.Fatalf("after delete: %+v", st)
	}
	h.stop()

	// Restart: nothing to redo, results identical.
	h = start(t, state, root, &embed.Fake{Dims: 768})
	h.idle()
	if hits := h.search("giraffes necks", 1); hits[0].Path != filepath.Join(root, "notes/zebra.md") {
		t.Fatalf("after restart: %+v", hits)
	}
	// Reindex rebuilds everything.
	if err := h.e.Reindex(); err != nil {
		t.Fatal(err)
	}
	h.idle()
	if st := h.e.Status(); st.Indexed != 4 || st.Store.Records == 0 || st.Store.Dead != 0 {
		t.Fatalf("after reindex: %+v", st)
	}
	// Dropping the root removes its files.
	if _, err := h.e.SetRoots(nil, nil); err != nil {
		t.Fatal(err)
	}
	h.idle()
	if st := h.e.Status(); st.Indexed != 0 || st.Failed != 0 {
		t.Fatalf("after removing root: %+v", st)
	}
	h.stop()
}

type otherModel struct{ embed.Fake }

func (o *otherModel) Info() embed.Info {
	i := o.Fake.Info()
	i.Precision = "q4"
	return i
}

func TestProfileChangeRequiresReindex(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	write(t, filepath.Join(root, "a.txt"), "alpha beta")
	h := start(t, state, root, &embed.Fake{Dims: 768})
	h.e.SetRoots([]string{root}, nil)
	h.idle()
	h.stop()

	h = start(t, state, root, &otherModel{embed.Fake{Dims: 768}})
	st := h.e.Status()
	if st.StoreError == "" || !strings.Contains(strings.Join(st.Paused, ","), pauseNoStore) {
		t.Fatalf("mismatch not surfaced: %+v", st)
	}
	if _, err := h.e.Search(context.Background(), SearchRequest{Q: "alpha", K: 1}); !errors.Is(err, ErrReindexRequired) {
		t.Fatalf("search err = %v", err)
	}
	if err := h.e.Reindex(); err != nil {
		t.Fatal(err)
	}
	h.idle()
	if hits := h.search("alpha", 1); len(hits) != 1 {
		t.Fatalf("after reindex: %+v", hits)
	}
	h.stop()
}

type flaky struct {
	embed.Fake
	fails int
}

func (f *flaky) EmbedTexts(ctx context.Context, p embed.Priority, texts []string) ([][]float32, error) {
	if p == embed.Index && f.fails > 0 {
		f.fails--
		return nil, embed.ErrUnavailable
	}
	return f.Fake.EmbedTexts(ctx, p, texts)
}

func TestTransientEmbedderFailureRetries(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	write(t, filepath.Join(root, "a.txt"), "alpha beta")
	h := start(t, state, root, &flaky{Fake: embed.Fake{Dims: 768}, fails: 1})
	h.e.SetRoots([]string{root}, nil)
	deadline := time.Now().Add(10 * time.Second)
	for h.e.Status().Indexed != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("never indexed after retry: %+v %+v", h.e.Status(), h.e.Failures())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if h.e.Status().Failed != 0 {
		t.Fatal("transient error recorded as failure")
	}
	h.stop()
}

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/msongd/kimquy/internal/client"
	"github.com/msongd/kimquy/internal/embed"
	"github.com/msongd/kimquy/internal/engine"
	"github.com/msongd/kimquy/internal/events"
	"github.com/msongd/kimquy/internal/kind"
	"github.com/msongd/kimquy/internal/store"
)

// serve starts an engine and the API on a Unix socket and returns a client.
func serve(t *testing.T) (*client.Client, string) {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng, err := engine.Open(engine.Options{StateDir: state, Embedder: &embed.Fake{Dims: 768}, Logger: log, Bus: events.NewBus()})
	if err != nil {
		t.Fatal(err)
	}
	// Unix socket paths are limited to ~104 bytes; t.TempDir can exceed that.
	sockDir, err := os.MkdirTemp("", "semd")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(sockDir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := &http.Server{Handler: Handler(eng, log), BaseContext: func(net.Listener) context.Context { return ctx }}
	go srv.Serve(ln)
	runDone := make(chan error, 1)
	go func() { runDone <- eng.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		srv.Close()
		if err := <-runDone; err != nil {
			t.Errorf("Run: %v", err)
		}
		if err := eng.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		os.RemoveAll(sockDir)
	})
	return client.NewClient(sock), dir
}

func waitIdle(t *testing.T, c *client.Client) client.Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for quiet := 0; quiet < 5; {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for idle")
		}
		time.Sleep(20 * time.Millisecond)
		s, err := c.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if s.Pending == 0 && !s.Scanning {
			quiet++
			if quiet == 5 {
				return s
			}
		} else {
			quiet = 0
		}
	}
	panic("unreachable")
}

func TestRoundTrip(t *testing.T) {
	c, dir := serve(t)
	ctx := context.Background()
	root := filepath.Join(dir, "docs")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "kv.md"), []byte("the quick brown fox jumps over the lazy dog"), 0o600)
	os.WriteFile(filepath.Join(root, "db.go"), []byte("package db // database connection pool tuning"), 0o600)
	os.WriteFile(filepath.Join(root, "bad.txt"), []byte("valid then \xc3\x28 invalid utf-8"), 0o600)

	r, err := c.SetRoots(ctx, RootsRequest{Roots: []string{root}, Excludes: []string{"*.log"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Roots) != 1 || len(r.Excludes) != 1 || len(r.DefaultExcludes) == 0 {
		t.Fatalf("roots: %+v", r)
	}
	st := waitIdle(t, c)
	if st.Indexed != 2 || st.Failed != 1 {
		t.Fatalf("status: %+v", st)
	}

	res, err := c.Search(ctx, SearchRequest{Q: "database pool"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) == 0 || filepath.Base(res.Hits[0].Path) != "db.go" || res.Hits[0].Snippet == "" || res.Hits[0].Score < 0.3 {
		t.Fatalf("hits: %+v", res.Hits)
	}
	res, err = c.Search(ctx, SearchRequest{Q: "database pool", Kinds: []string{"office"}})
	if err != nil || len(res.Hits) != 0 {
		t.Fatalf("kind filter: %v %+v", err, res.Hits)
	}

	f, err := c.Failures(ctx)
	if err != nil || len(f) != 1 || filepath.Base(f[0].Path) != "bad.txt" {
		t.Fatalf("failures: %v %+v", err, f)
	}

	if err := c.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.Status(ctx); len(st.Paused) == 0 {
		t.Fatalf("not paused: %+v", st)
	}
	if err := c.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Reindex(ctx); err != nil {
		t.Fatal(err)
	}
	if st := waitIdle(t, c); st.Indexed != 2 {
		t.Fatalf("after reindex: %+v", st)
	}
}

func TestValidation(t *testing.T) {
	c, _ := serve(t)
	ctx := context.Background()
	bad := []SearchRequest{
		{Q: ""},
		{Q: "   "},
		{Q: strings.Repeat("x", 2001)},
		{Q: "x", K: 101},
		{Q: "x", K: -1},
		{Q: "x", Kinds: []string{"nope"}},
		{Q: "x", Roots: []string{"relative/dir"}},
	}
	for _, req := range bad {
		if _, err := c.Search(ctx, req); err == nil || !strings.Contains(err.Error(), "HTTP 400") {
			t.Errorf("%+v: want 400, got %v", req, err)
		}
	}
	if _, err := c.SetRoots(ctx, RootsRequest{Roots: []string{"relative"}}); err == nil {
		t.Error("relative root accepted")
	}
	if _, err := c.SetRoots(ctx, RootsRequest{}); err == nil {
		t.Error("missing roots accepted")
	}

	// Raw bodies the typed client cannot produce.
	raw := []string{
		`{"q":"x","extra":1}`,
		`{"q":"x"} {"q":"y"}`,
		`not json`,
		`{"q":"` + strings.Repeat("x", maxBody) + `"}`,
	}
	for _, body := range raw {
		req, _ := http.NewRequest(http.MethodPost, "http://semd/v1/search", strings.NewReader(body))
		resp, err := c.HTTP().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%.40q: status %d", body, resp.StatusCode)
		}
	}
	resp, err := c.HTTP().Get("http://semd/v1/search")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET search: %d", resp.StatusCode)
	}
}

func TestPreview(t *testing.T) {
	c, dir := serve(t)
	ctx := context.Background()
	root := filepath.Join(dir, "docs")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	root, _ = filepath.EvalSymlinks(root)
	note := filepath.Join(root, "kv.md")
	os.WriteFile(note, []byte("the quick brown fox jumps over the lazy dog"), 0o600)
	var img bytes.Buffer
	png.Encode(&img, image.NewGray(image.Rect(0, 0, 8, 4)))
	pic := filepath.Join(root, "pic.png")
	os.WriteFile(pic, img.Bytes(), 0o600)
	if _, err := c.SetRoots(ctx, RootsRequest{Roots: []string{root}}); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, c)

	p, err := c.Preview(ctx, PreviewRequest{Path: note, Locator: kind.Locator{Kind: kind.LocBytes, Start: 4, End: 9}})
	if err != nil || p.Text[p.MatchStart:p.MatchEnd] != "quick" || p.Kind != "text" || p.Vectors != 1 || p.Modified.IsZero() {
		t.Fatalf("preview: %v %+v", err, p)
	}
	got, err := c.Thumbnail(ctx, pic)
	if err != nil || !bytes.Equal(got, img.Bytes()) {
		t.Fatalf("thumbnail: %v, %d bytes", err, len(got))
	}

	for _, tc := range []struct {
		req  PreviewRequest
		code string
	}{
		{PreviewRequest{Path: filepath.Join(root, "nope.md")}, "HTTP 404"},
		{PreviewRequest{Path: "relative.md"}, "HTTP 400"},
		{PreviewRequest{Path: note, Locator: kind.Locator{Kind: kind.LocTime, Start: 0, End: 1}}, "HTTP 400"},
	} {
		if _, err := c.Preview(ctx, tc.req); err == nil || !strings.Contains(err.Error(), tc.code) {
			t.Errorf("%+v: want %s, got %v", tc.req, tc.code, err)
		}
	}
	if _, err := c.Thumbnail(ctx, note); err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("thumbnail of text: %v", err)
	}
	for _, q := range []string{"", "?path=a&path=b", "?path=" + pic + "&x=1"} {
		resp, err := c.HTTP().Get("http://semd/v1/thumbnail" + q)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("thumbnail%s: status %d", q, resp.StatusCode)
		}
	}
}

func TestEventsStream(t *testing.T) {
	c, dir := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got := make(chan events.Event, 64)
	errc := make(chan error, 1)
	go func() { errc <- c.Events(ctx, func(ev events.Event) { got <- ev }) }()
	next := func(typ string) events.Event {
		t.Helper()
		for {
			select {
			case ev := <-got:
				if ev.Type == typ {
					return ev
				}
			case <-ctx.Done():
				t.Fatalf("no %s event", typ)
			}
		}
	}
	next("open") // subscribed: nothing after this is missed
	root := filepath.Join(dir, "r")
	os.MkdirAll(root, 0o700)
	if _, err := c.SetRoots(ctx, RootsRequest{Roots: []string{root}}); err != nil {
		t.Fatal(err)
	}
	next("scan")
	if err := c.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if ev := next("pause"); ev.Msg == "resumed" {
		t.Fatalf("pause: %+v", ev)
	}
	if err := c.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if ev := next("pause"); ev.Msg != "resumed" {
		t.Fatalf("resume: %+v", ev)
	}
	cancel()
	if err := <-errc; err != context.Canceled {
		t.Fatalf("Events: %v", err)
	}
}

// mirror checks that the client's type for a reply carries exactly the
// fields of the engine's: from must have every field set.
func mirror(t *testing.T, from, to any) {
	t.Helper()
	want, err := json.Marshal(from)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(want))
	dec.DisallowUnknownFields()
	if err := dec.Decode(to); err != nil {
		t.Fatalf("client %T lacks a field of %T: %v", to, from, err)
	}
	got, _ := json.Marshal(to)
	var a, b any
	json.Unmarshal(want, &a)
	json.Unmarshal(got, &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("%T and %T differ:\n engine %s\n client %s", from, to, want, got)
	}
}

func TestClientMirrorsEngine(t *testing.T) {
	loc := kind.Locator{Kind: kind.LocTime, Start: 1000, End: 2000}
	mirror(t, SearchResponse{Hits: []engine.Hit{{Path: "/a", Kind: "audio", Locator: loc, Score: .5, Snippet: "s", Stale: true}}, TookMS: 3},
		&client.SearchResponse{})
	mirror(t, engine.Status{Indexed: 1, Pending: 2, Failed: 3, Scanning: true, Watch: "live", Paused: []string{"manual"},
		Embedder: engine.EmbedStatus{Model: "m", Precision: "p", Dims: 4},
		Store:    &store.Stats{Segments: 1, Records: 2, Dead: 3, Files: 4}, StoreError: "e",
		Roots: []engine.RootStatus{{Path: "/r", Error: "EPERM"}}}, &client.Status{})
	mirror(t, engine.Preview{Path: "/p", Kind: "pdf", Locator: kind.Locator{Kind: kind.LocPage, Page: 2, Start: 1, End: 4},
		Size: 9, Modified: time.Unix(1, 0).UTC(), Indexed: time.Unix(2, 0).UTC(), Status: "indexed", Vectors: 3,
		SHA256: "ab", Stale: true, Pages: 4, Duration: 5, Width: 6, Height: 7,
		Text: "text", TextStart: 8, TextTotal: 20, MatchStart: 1, MatchEnd: 3, TextError: "e"}, &client.Preview{})
	mirror(t, []engine.Failure{{Path: "/f", Error: "bad"}}, &[]client.Failure{})
}

func TestSettings(t *testing.T) {
	c, _ := serve(t)
	ctx := context.Background()
	s, err := c.Settings(ctx)
	if err != nil || !s.PauseOnBattery {
		t.Fatalf("default settings %+v %v", s, err)
	}
	if s, err = c.SetSettings(ctx, client.Settings{PauseOnBattery: false}); err != nil || s.PauseOnBattery {
		t.Fatalf("set: %+v %v", s, err)
	}
	if s, err = c.Settings(ctx); err != nil || s.PauseOnBattery {
		t.Fatalf("after set: %+v %v", s, err)
	}
}

package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixture swaps Parts for two small synthetic parts and returns their
// contents by "<part>/<file>".
func fixture(t *testing.T) map[string][]byte {
	t.Helper()
	data := map[string][]byte{}
	mk := func(dir string, required bool, files map[string]int) Part {
		p := Part{Dir: dir, Label: dir, Required: required}
		for _, name := range []string{"weights.bin", "manifest.json"} {
			b := bytes.Repeat([]byte(dir+"/"+name+";"), files[name])
			data[dir+"/"+name] = b
			s := sha256.Sum256(b)
			p.Files = append(p.Files, File{name, int64(len(b)), hex.EncodeToString(s[:])})
		}
		return p
	}
	old := Parts
	Parts = []Part{
		mk("text-q8", true, map[string]int{"weights.bin": 5000, "manifest.json": 3}),
		mk("vision-q8", false, map[string]int{"weights.bin": 3000, "manifest.json": 2}),
	}
	t.Cleanup(func() { Parts = old })
	return data
}

// server serves data under /resolve/<part>/<file> by redirecting to
// /cdn/<part>/<file>, as Hugging Face does. serve may change a response.
type server struct {
	*httptest.Server
	mu     sync.Mutex
	ranges []string // Range headers the CDN saw
	serve  func(w http.ResponseWriter, r *http.Request, b []byte) bool
}

func newServer(t *testing.T, data map[string][]byte) *server {
	s := &server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rest, ok := strings.CutPrefix(r.URL.Path, "/resolve/"); ok {
			http.Redirect(w, r, "/cdn/"+rest, http.StatusFound)
			return
		}
		b, ok := data[strings.TrimPrefix(r.URL.Path, "/cdn/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		s.ranges = append(s.ranges, r.Header.Get("Range"))
		serve := s.serve
		s.mu.Unlock()
		if serve != nil && serve(w, r, b) {
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
	}))
	t.Cleanup(s.Close)
	return s
}

func installer(t *testing.T, s *server) *Installer {
	in := NewInstaller(filepath.Join(t.TempDir(), "models"))
	in.Base, in.Client, in.Idle = s.URL+"/resolve", s.Client(), 5*time.Second
	return in
}

func wantInstalled(t *testing.T, dir string, want ...string) {
	t.Helper()
	have, err := Installed(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range Parts {
		if got := have[p.Dir]; got != slices.Contains(want, p.Dir) {
			t.Errorf("%s installed = %v, want %v", p.Dir, got, !got)
		}
		if have[p.Dir] {
			if err := Verify(dir, p, nil); err != nil {
				t.Error(err)
			}
		}
	}
	// A failed download keeps what it staged, to resume from.
	if _, err := os.Stat(filepath.Join(dir, staging)); err == nil && len(want) > 0 {
		t.Error("staging folder left behind")
	}
}

func TestDownload(t *testing.T) {
	data := fixture(t)
	s := newServer(t, data)
	in := installer(t, s)
	var last, total int64
	err := in.Download(context.Background(), Parts, func(d, n int64) { last, total = d, n })
	if err != nil {
		t.Fatal(err)
	}
	wantInstalled(t, in.Dir, "text-q8", "vision-q8")
	if want := Parts[0].Size() + Parts[1].Size(); last != want || total != want {
		t.Errorf("progress ended at %d of %d, want %d", last, total, want)
	}
	// Nothing to do the second time.
	s.ranges = nil
	if err := in.Download(context.Background(), Parts, nil); err != nil || len(s.ranges) != 0 {
		t.Errorf("second download: %v, %d requests", err, len(s.ranges))
	}
}

func TestDownloadOnePart(t *testing.T) {
	s := newServer(t, fixture(t))
	in := installer(t, s)
	if err := in.Download(context.Background(), Parts[:1], nil); err != nil {
		t.Fatal(err)
	}
	wantInstalled(t, in.Dir, "text-q8")
	if err := in.Download(context.Background(), Parts[1:], nil); err != nil {
		t.Fatal(err)
	}
	wantInstalled(t, in.Dir, "text-q8", "vision-q8")
}

// stage leaves the first n bytes of a file as an interrupted download would.
func stage(t *testing.T, in *Installer, data map[string][]byte, key string, n int) {
	t.Helper()
	part, name, _ := strings.Cut(key, "/")
	dir := filepath.Join(in.Dir, staging, part)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".part"), data[key][:n], 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadResumes(t *testing.T) {
	data := fixture(t)
	s := newServer(t, data)
	in := installer(t, s)
	stage(t, in, data, "text-q8/weights.bin", 1234)
	var first int64 = -1
	err := in.Download(context.Background(), Parts[:1], func(d, _ int64) {
		if first < 0 {
			first = d
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	wantInstalled(t, in.Dir, "text-q8")
	if first != 1234 {
		t.Errorf("progress started at %d, want 1234", first)
	}
	if !slices.Contains(s.ranges, "bytes=1234-") {
		t.Errorf("ranges %q, want a resume from 1234", s.ranges)
	}
}

func TestDownloadRestartsWhenRangeIgnored(t *testing.T) {
	data := fixture(t)
	s := newServer(t, data)
	s.serve = func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Write(b) // 200, the whole file
		return true
	}
	in := installer(t, s)
	stage(t, in, data, "text-q8/weights.bin", 1234)
	if err := in.Download(context.Background(), Parts[:1], nil); err != nil {
		t.Fatal(err)
	}
	wantInstalled(t, in.Dir, "text-q8")
}

func TestDownloadRejectsBadContentRange(t *testing.T) {
	data := fixture(t)
	s := newServer(t, data)
	s.serve = func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		if r.Header.Get("Range") == "" {
			return false
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(b)-1, len(b)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(b)
		return true
	}
	in := installer(t, s)
	stage(t, in, data, "text-q8/weights.bin", 1234)
	err := in.Download(context.Background(), Parts[:1], nil)
	if err == nil || !strings.Contains(err.Error(), "Content-Range") {
		t.Fatalf("err = %v, want a Content-Range error", err)
	}
	wantInstalled(t, in.Dir)
}

func TestDownloadBadHash(t *testing.T) {
	data := fixture(t)
	s := newServer(t, data)
	s.serve = func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		if !strings.HasSuffix(r.URL.Path, "weights.bin") {
			return false
		}
		bad := bytes.Clone(b)
		bad[len(bad)/2] ^= 1
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(bad))
		return true
	}
	in := installer(t, s)
	err := in.Download(context.Background(), Parts[:1], nil)
	if err == nil || !strings.Contains(err.Error(), "not the expected file") {
		t.Fatalf("err = %v, want a hash mismatch", err)
	}
	wantInstalled(t, in.Dir)
	if in.staged(Parts[0], Parts[0].Files[0]) != 0 {
		t.Error("the corrupt download was kept")
	}
	// Fixed upstream: the next try succeeds.
	s.serve = nil
	if err := in.Download(context.Background(), Parts[:1], nil); err != nil {
		t.Fatal(err)
	}
	wantInstalled(t, in.Dir, "text-q8")
}

func TestDownloadHTTPError(t *testing.T) {
	s := newServer(t, fixture(t))
	s.serve = func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		http.Error(w, "nope", http.StatusForbidden)
		return true
	}
	in := installer(t, s)
	if err := in.Download(context.Background(), Parts[:1], nil); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want 403", err)
	}
}

func TestDownloadStalls(t *testing.T) {
	data := fixture(t)
	s := newServer(t, data)
	release := make(chan struct{})
	defer close(release)
	s.serve = func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		if !strings.HasSuffix(r.URL.Path, "weights.bin") {
			return false
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Write(b[:100])
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
		return true
	}
	in := installer(t, s)
	in.Idle = 200 * time.Millisecond
	start := time.Now()
	err := in.Download(context.Background(), Parts[:1], nil)
	if !errors.Is(err, errStalled) {
		t.Fatalf("err = %v, want stalled", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("took %v to notice", d)
	}
	if got := in.staged(Parts[0], Parts[0].Files[0]); got != 100 {
		t.Errorf("kept %d bytes to resume from, want 100", got)
	}
}

func TestDownloadCancel(t *testing.T) {
	data := fixture(t)
	s := newServer(t, data)
	ctx, cancel := context.WithCancel(context.Background())
	s.serve = func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		cancel()
		return false
	}
	in := installer(t, s)
	if err := in.Download(ctx, Parts, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}
	wantInstalled(t, in.Dir)
}

func TestDownloadReplacesDamagedPart(t *testing.T) {
	s := newServer(t, fixture(t))
	in := installer(t, s)
	if err := in.Download(context.Background(), Parts[:1], nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(in.Dir, "text-q8", "weights.bin"), 10); err != nil {
		t.Fatal(err)
	}
	if _, err := Installed(in.Dir); err == nil {
		t.Fatal("a damaged part passed the check")
	}
	if err := in.Download(context.Background(), Parts[:1], nil); err != nil {
		t.Fatal(err)
	}
	wantInstalled(t, in.Dir, "text-q8")
}

func TestDownloadChecksArguments(t *testing.T) {
	s := newServer(t, fixture(t))
	in := installer(t, s)
	bogus := Parts[0]
	bogus.Files = bogus.Files[:1]
	for name, f := range map[string]func() error{
		"relative dir": func() error {
			in := *in
			in.Dir = "models"
			return in.Download(context.Background(), Parts, nil)
		},
		"no parts":     func() error { return in.Download(context.Background(), nil, nil) },
		"unknown part": func() error { return in.Download(context.Background(), []Part{bogus}, nil) },
		"no client": func() error {
			in := *in
			in.Client = nil
			return in.Download(context.Background(), Parts, nil)
		},
	} {
		if err := f(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestDownloadLocked(t *testing.T) {
	s := newServer(t, fixture(t))
	in := installer(t, s)
	l, err := in.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := in.Download(context.Background(), Parts, nil); err == nil || !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("err = %v, want in progress", err)
	}
}

// installed writes the parts' files into dir as a finished model would be.
func installed(t *testing.T, data map[string][]byte, dir string, parts ...Part) {
	t.Helper()
	for _, p := range parts {
		for _, f := range p.Files {
			path := filepath.Join(dir, p.Dir, f.Name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data[p.Dir+"/"+f.Name], 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestImport(t *testing.T) {
	data := fixture(t)
	root := t.TempDir()
	app := filepath.Join(root, "Old.app")
	installed(t, data, filepath.Join(app, "Contents", "Resources", "models"), Parts...)
	loose := filepath.Join(root, "loose")
	installed(t, data, loose, Parts[0])

	for name, src := range map[string]string{
		"app":         app,
		"model dir":   filepath.Join(app, "Contents", "Resources", "models"),
		"part folder": filepath.Join(app, "Contents", "Resources", "models", "vision-q8"),
	} {
		t.Run(name, func(t *testing.T) {
			in := NewInstaller(filepath.Join(t.TempDir(), "models"))
			var last, total int64
			got, err := in.Import(context.Background(), src, func(d, n int64) { last, total = d, n })
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 {
				t.Errorf("imported %d parts, want 2", len(got))
			}
			wantInstalled(t, in.Dir, "text-q8", "vision-q8")
			if last != total || total != Parts[0].Size()+Parts[1].Size() {
				t.Errorf("progress ended at %d of %d", last, total)
			}
		})
	}

	t.Run("adds only what is missing", func(t *testing.T) {
		in := NewInstaller(filepath.Join(t.TempDir(), "models"))
		if _, err := in.Import(context.Background(), loose, nil); err != nil {
			t.Fatal(err)
		}
		got, err := in.Import(context.Background(), app, nil)
		if err != nil || len(got) != 1 || got[0].Dir != "vision-q8" {
			t.Fatalf("got %v, %v; want vision-q8 alone", got, err)
		}
		wantInstalled(t, in.Dir, "text-q8", "vision-q8")
	})
}

func TestImportRefuses(t *testing.T) {
	data := fixture(t)
	root := t.TempDir()
	model := filepath.Join(root, "models")
	installed(t, data, model, Parts[0])
	damaged := filepath.Join(root, "damaged")
	installed(t, data, damaged, Parts[0])
	if err := os.WriteFile(filepath.Join(damaged, "text-q8", "weights.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	wrong := filepath.Join(root, "wrong")
	installed(t, data, wrong, Parts[0])
	w := filepath.Join(wrong, "text-q8", "weights.bin")
	b, _ := os.ReadFile(w)
	b[0] ^= 1
	if err := os.WriteFile(w, b, 0o644); err != nil {
		t.Fatal(err)
	}

	for name, c := range map[string]struct{ src, dir, want string }{
		"empty folder": {root + "/nothing", filepath.Join(root, "a"), "no such file"},
		"no model":     {root, filepath.Join(root, "b"), "no EmbeddingGemma 2 model"},
		"itself":       {model, model, "already in use"},
		"damaged":      {damaged, filepath.Join(root, "c"), "damaged"},
		"other bytes":  {wrong, filepath.Join(root, "d"), "not the expected file"},
	} {
		t.Run(name, func(t *testing.T) {
			if name == "empty folder" {
				os.Mkdir(c.src, 0o755)
				c.want = "no EmbeddingGemma 2 model"
			}
			_, err := NewInstaller(c.dir).Import(context.Background(), c.src, nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if c.dir != model {
				if have, _ := Installed(c.dir); have["text-q8"] {
					t.Error("installed anyway")
				}
			}
		})
	}
}

func TestFind(t *testing.T) {
	data := fixture(t)
	root := t.TempDir()
	semd := filepath.Join(root, "Kim Quy.app", "Contents", "Helpers", "semd")
	state := filepath.Join(root, "state")
	if got := Find(semd, state); got != filepath.Join(state, "models") {
		t.Errorf("slim app: %s", got)
	}
	bundled := filepath.Join(root, "Kim Quy.app", "Contents", "Resources", "models")
	installed(t, data, bundled, Parts[0])
	if got := Find(semd, state); got != bundled {
		t.Errorf("bundled app: %s", got)
	}
}

func TestBytes(t *testing.T) {
	for n, want := range map[int64]string{
		12: "12 bytes", 1500: "2 KB", 305_000_000: "305 MB", 1_072_000_000: "1.1 GB",
	} {
		if got := Bytes(n); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// The pinned manifest must stay self-consistent: one required part, first.
func TestParts(t *testing.T) {
	if !Parts[0].Required || Parts[0].Dir != "text-q8" {
		t.Fatal("text-q8 must come first and be required")
	}
	for _, p := range Parts {
		seen := map[string]bool{}
		for _, f := range p.Files {
			if seen[f.Name] || f.Size <= 0 || len(f.SHA256) != 64 {
				t.Errorf("%s/%s: bad entry", p.Dir, f.Name)
			}
			seen[f.Name] = true
		}
		if !seen["manifest.json"] {
			t.Errorf("%s has no manifest.json", p.Dir)
		}
	}
}

package engine

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/msongd/kimquy/internal/embed"
	"github.com/msongd/kimquy/internal/kind"
)

func TestWindow(t *testing.T) {
	lines := strings.Repeat("0123456789\n", 10) // 110 bytes, lines of 11
	for _, tc := range []struct {
		name         string
		text         string
		s, end, n    int
		want         string
		matchInsideS int // where s lands in the window
	}{
		{"fits", "short text", 2, 5, 100, "short text", 2},
		{"no match", lines, 0, 0, 30, "0123456789\n0123456789\n", 0},
		// Centered on [55,60): 25 bytes either side, then cut to whole lines.
		{"centered", lines, 55, 60, 60, lines[33:88], 55 - 33},
		{"at end", lines, 105, 110, 30, lines[88:], 105 - 88},
		{"long match", lines, 11, 90, 30, lines[11:41], 0},
	} {
		lo, hi := window(tc.text, tc.s, tc.end, tc.n)
		if got := tc.text[lo:hi]; got != tc.want || tc.s-lo != tc.matchInsideS {
			t.Errorf("%s: window [%d,%d) = %q, match at %d; want %q at %d", tc.name, lo, hi, got, tc.s-lo, tc.want, tc.matchInsideS)
		}
		if hi-lo > tc.n {
			t.Errorf("%s: %d bytes, more than %d", tc.name, hi-lo, tc.n)
		}
	}
	// Multibyte text without line ends: cut on rune boundaries.
	text := strings.Repeat("é", 50) // 100 bytes
	for s := 0; s < len(text); s += 2 {
		lo, hi := window(text, s, s+2, 15)
		if !utf8.ValidString(text[lo:hi]) || lo > s || hi < s+2 || hi-lo > 15 {
			t.Fatalf("match at %d: window [%d,%d) %q", s, lo, hi, text[lo:hi])
		}
	}
}

func pngFile(t *testing.T, path string, w, h int) {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPreview(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	var body strings.Builder
	for i := range 3000 { // ~100 KB: more than one preview holds
		body.WriteString("filler line about nothing much at all " + string(rune('a'+i%26)) + "\n")
	}
	body.WriteString("The lighthouse keeper trims the lamp wick every evening at dusk.\n")
	for range 3000 {
		body.WriteString("more filler after the interesting part\n")
	}
	long := filepath.Join(root, "notes/long.txt")
	write(t, long, body.String())
	pages := []string{"Chapter one explains how glaciers carve valleys over thousands of years.", ""}
	pdf := filepath.Join(root, "geology.pdf")
	write(t, pdf, strings.Join(pages, "\f"))
	photo := filepath.Join(root, "photos/harbour.png")
	pngFile(t, photo, 40, 30)
	gone := filepath.Join(root, "gone.txt")
	write(t, gone, "a note about a file that will be deleted soon")

	h := start(t, state, root, &embed.Fake{Dims: 768})
	defer h.stop()
	if _, err := h.e.SetRoots([]string{root}, nil); err != nil {
		t.Fatal(err)
	}
	h.idle()
	ctx := context.Background()

	whole := body.String()
	at := strings.Index(whole, "The lighthouse")
	sentence := "The lighthouse keeper trims the lamp wick every evening at dusk."
	p, err := h.e.Preview(ctx, long, kind.Locator{Kind: kind.LocBytes, Start: int64(at), End: int64(at + len(sentence))})
	if err != nil {
		t.Fatal(err)
	}
	match := p.Text[p.MatchStart:p.MatchEnd]
	if match != sentence || len(p.Text) > previewBytes || p.TextTotal != int64(len(whole)) ||
		whole[p.TextStart:p.TextStart+int64(len(p.Text))] != p.Text || p.TextStart == 0 ||
		!strings.HasSuffix(p.Text, "\n") || p.TextStart > 0 && whole[p.TextStart-1] != '\n' {
		t.Fatalf("text window [%d,+%d) of %d, match %q", p.TextStart, len(p.Text), p.TextTotal, match)
	}
	if p.Kind != "text" || p.Status != "indexed" || p.Vectors < 2 || len(p.SHA256) != 64 || p.Stale ||
		p.Size != int64(len(whole)) || p.Modified.IsZero() || time.Since(p.Indexed) > time.Minute {
		t.Fatalf("details %+v", p)
	}

	// Without a locator: the text from its start.
	if p, err = h.e.Preview(ctx, long, kind.Locator{}); err != nil || p.TextStart != 0 || p.MatchEnd != 0 ||
		!strings.HasPrefix(p.Text, "filler line") {
		t.Fatalf("no locator: %v %q…", err, p.Text[:20])
	}

	// A PDF: the page's text, and for a page searched as an image, why none.
	p, err = h.e.Preview(ctx, pdf, kind.Locator{Kind: kind.LocPage, Page: 1, Start: 8, End: 11})
	if err != nil || p.Pages != 2 || p.Text != pages[0] || p.Text[p.MatchStart:p.MatchEnd] != "one" {
		t.Fatalf("pdf: %v %+v", err, p)
	}
	p, err = h.e.Preview(ctx, pdf, kind.Locator{Kind: kind.LocPage, Page: 2})
	if err != nil || p.Text != "" || !strings.Contains(p.TextError, "picture") {
		t.Fatalf("pdf image page: %v %+v", err, p)
	}

	p, err = h.e.Preview(ctx, photo, kind.Locator{})
	if err != nil || p.Width != 40 || p.Height != 30 || p.Kind != "image" {
		t.Fatalf("image: %v %+v", err, p)
	}

	// Deleted after indexing: its details as indexed, marked stale.
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if p, err = h.e.Preview(ctx, gone, kind.Locator{}); err != nil || !p.Stale || p.TextError == "" || !p.Modified.IsZero() {
		t.Fatalf("gone: %v %+v", err, p)
	}

	for _, tc := range []struct {
		path string
		loc  kind.Locator
		want error
	}{
		{filepath.Join(root, "nope.txt"), kind.Locator{}, ErrNotIndexed},
		{"relative.txt", kind.Locator{}, ErrBadPreview},
		{root + "/notes/../notes/long.txt", kind.Locator{}, ErrBadPreview},
		{long, kind.Locator{Kind: kind.LocBytes, Start: 5, End: 2}, ErrBadPreview},
		{long, kind.Locator{Kind: kind.LocTime, Start: 0, End: 2}, ErrBadPreview},
		{photo, kind.Locator{Kind: kind.LocPage, Page: 1}, ErrBadPreview},
	} {
		if _, err := h.e.Preview(ctx, tc.path, tc.loc); !errors.Is(err, tc.want) {
			t.Errorf("Preview(%s, %v) = %v, want %v", tc.path, tc.loc, err, tc.want)
		}
	}
}

// A preview waits for a busy embedder only so long, and still comes.
func TestPreviewBudget(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	pdf := filepath.Join(root, "islands.pdf")
	write(t, pdf, "Chapter three covers volcanic islands and lava tubes in great detail.")
	h := start(t, state, root, slowPDF{&embed.Fake{Dims: 768}})
	defer h.stop()
	if _, err := h.e.SetRoots([]string{root}, nil); err != nil {
		t.Fatal(err)
	}
	h.idle()
	old := previewBudget
	previewBudget = 100 * time.Millisecond
	defer func() { previewBudget = old }()
	p, err := h.e.Preview(context.Background(), pdf, kind.Locator{Kind: kind.LocPage, Page: 1})
	if err != nil || !strings.Contains(p.TextError, "busy") || p.Size == 0 {
		t.Fatalf("%v %+v", err, p)
	}
}

func TestThumbnail(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	small := filepath.Join(root, "small.png")
	pngFile(t, small, 20, 10)
	wide := filepath.Join(root, "wide.png")
	pngFile(t, wide, thumbRawPixels+1, 2)
	heic := filepath.Join(root, "phone.heic")
	write(t, heic, "not really a heic")
	text := filepath.Join(root, "a.txt")
	write(t, text, "some words to index")

	// A stand-in for sips: writes "scaled" to the path after --out.
	bin := t.TempDir()
	fake := filepath.Join(bin, "sips")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nfor a; do out=$a; done\nprintf scaled > \"$out\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := sips
	sips = fake
	defer func() { sips = old }()

	h := start(t, state, root, &embed.Fake{Dims: 768})
	defer h.stop()
	if _, err := h.e.SetRoots([]string{root}, nil); err != nil {
		t.Fatal(err)
	}
	h.idle()
	ctx := context.Background()

	want, _ := os.ReadFile(small)
	if data, typ, err := h.e.Thumbnail(ctx, small); err != nil || typ != "image/png" || !bytes.Equal(data, want) {
		t.Fatalf("small: %v %s %d bytes", err, typ, len(data))
	}
	for _, p := range []string{wide, heic} {
		if data, typ, err := h.e.Thumbnail(ctx, p); err != nil || typ != "image/jpeg" || string(data) != "scaled" {
			t.Fatalf("%s: %v %s %q", filepath.Base(p), err, typ, data)
		}
	}
	sips = filepath.Join(bin, "missing")
	if _, _, err := h.e.Thumbnail(ctx, heic); err == nil || !strings.Contains(err.Error(), "sips") {
		t.Fatalf("no sips: %v", err)
	}
	if _, _, err := h.e.Thumbnail(ctx, text); !errors.Is(err, ErrBadPreview) {
		t.Fatalf("text file: %v", err)
	}
	if _, _, err := h.e.Thumbnail(ctx, filepath.Join(root, "nope.png")); !errors.Is(err, ErrNotIndexed) {
		t.Fatalf("not indexed: %v", err)
	}
}

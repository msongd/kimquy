package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msongd/kimquy/internal/embed"
	"github.com/msongd/kimquy/internal/kind"
)

// Images and PDFs go through the media embedder. The fake reads PDFs as
// form-feed separated text and embeds an image by its file name.
func TestMedia(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	write(t, filepath.Join(root, "photos/sunset-beach.jpg"), "not really a jpeg")
	pages := []string{
		"Chapter one explains how glaciers carve valleys over thousands of years.",
		"", // a scanned page: rendered as an image
		"Chapter three covers volcanic islands and lava tubes in great detail.",
	}
	write(t, filepath.Join(root, "geology.pdf"), strings.Join(pages, "\f"))
	// More text-less pages than the render cap: indexed, with the rest reported.
	write(t, filepath.Join(root, "scans.pdf"), strings.Repeat("\f", maxRenderedPages+2))
	// Not valid for the fake: fails the file, not the daemon.
	write(t, filepath.Join(root, "broken.pdf"), "\xff\xfe")

	h := start(t, state, root, &embed.Fake{Dims: 768})
	if _, err := h.e.SetRoots([]string{root}, nil); err != nil {
		t.Fatal(err)
	}
	h.idle()

	if st := h.e.Status(); st.Indexed != 3 || st.Failed != 1 {
		t.Fatalf("status %+v, failures %+v", st, h.e.Failures())
	}
	if f := h.e.Failures(); !strings.HasSuffix(f[0].Path, "broken.pdf") {
		t.Fatalf("failures %+v", f)
	}

	hits := h.search("sunset", 1)
	if hits[0].Path != filepath.Join(root, "photos/sunset-beach.jpg") || hits[0].Kind != "image" ||
		hits[0].Locator.Kind != kind.LocNone || hits[0].Snippet != "" {
		t.Fatalf("image hit %+v", hits[0])
	}

	hits = h.search("volcanic islands lava", 1)
	want := kind.Locator{Kind: kind.LocPage, Page: 3, Start: 0, End: int64(len(pages[2]))}
	if hits[0].Path != filepath.Join(root, "geology.pdf") || hits[0].Locator != want ||
		!strings.HasPrefix(hits[0].Snippet, "Chapter three") {
		t.Fatalf("pdf text hit %+v, want locator %v", hits[0], want)
	}

	// The scanned page 2 was embedded as an image ("geology" + its empty text).
	hits = h.search("geology", 1)
	if hits[0].Path != filepath.Join(root, "geology.pdf") || hits[0].Locator != (kind.Locator{Kind: kind.LocPage, Page: 2}) {
		t.Fatalf("pdf image hit %+v", hits[0])
	}

	f, ok := h.e.cat.Lookup(filepath.Join(root, "scans.pdf"))
	if !ok || f.NVec != maxRenderedPages {
		t.Fatalf("scans.pdf: %+v, want %d vectors", f, maxRenderedPages)
	}
	h.stop()
}

func TestPDFRequestPaging(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	// More pages than one extract_pdf call returns, each with distinct text.
	var pages []string
	for i := range 3*embed.MaxPDFPagesPerCall + 1 {
		pages = append(pages, fmt.Sprintf("Page about topic%d with enough words to count as text, page %d.", i, i))
	}
	write(t, filepath.Join(root, "long.pdf"), strings.Join(pages, "\f"))
	h := start(t, state, root, &embed.Fake{Dims: 768})
	if _, err := h.e.SetRoots([]string{root}, nil); err != nil {
		t.Fatal(err)
	}
	h.idle()
	for _, i := range []int{0, embed.MaxPDFPagesPerCall, len(pages) - 1} {
		hits := h.search(fmt.Sprintf("topic%d", i), 1)
		if hits[0].Locator.Page != uint16(i+1) {
			t.Fatalf("topic%d: %+v", i, hits[0])
		}
	}
	h.stop()
}

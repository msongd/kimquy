package main

import (
	"bytes"
	"errors"
	"image"
	"image/png"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/msongd/kimquy/internal/client"
)

const pdfText = "Summary\nThe quarterly revenue grew by a fifth.\nCosts held steady.\n"

// quickHarness has searched "revenue" and shows the report and the talk.
func quickHarness(t *testing.T) *harness {
	h := newHarness(t)
	h.semd.hits["revenue"] = []client.Hit{pdfHit, talkHit}
	at := strings.Index(pdfText, "quarterly revenue")
	h.semd.previews = map[string]client.Preview{
		pdfHit.Path: {Path: pdfHit.Path, Kind: "pdf", Locator: pdfHit.Locator, Size: 2_400_000, Pages: 12,
			Status: "indexed", Vectors: 40, SHA256: strings.Repeat("ab", 32),
			Modified: time.Date(2026, 3, 1, 9, 30, 0, 0, time.Local), Indexed: time.Date(2026, 3, 2, 10, 0, 0, 0, time.Local),
			Text: pdfText, TextTotal: int64(len(pdfText)), MatchStart: at, MatchEnd: at + len("quarterly revenue")},
		talkHit.Path: {Path: talkHit.Path, Kind: "audio", Locator: talkHit.Locator, Size: 900,
			Duration: 3_800_000, Status: "indexed", Vectors: 7, Stale: true},
	}
	h.Type("revenue")
	h.Advance(debounce)
	h.flush()
	h.want("report.pdf")
	return h
}

func TestQuickLookFromRow(t *testing.T) {
	h := quickHarness(t)
	if h.HasText("Close Quick Look") {
		t.Fatal("pane open before asked")
	}
	h.click("Quick Look at keynote.m4a")
	h.flush()
	if h.a.chosen != 1 || !h.a.quick.open {
		t.Fatalf("chosen %d, open %v", h.a.chosen, h.a.quick.open)
	}
	if len(h.semd.previewed) != 1 || h.semd.previewed[0] != (client.PreviewRequest{Path: talkHit.Path, Locator: talkHit.Locator}) {
		t.Fatalf("previewed %v", h.semd.previewed)
	}
	for _, s := range []string{"The file changed since it was indexed", "The match is at 1:05–1:02:05",
		"1:03:20", "900 bytes", "~/Talks", "Show in Finder"} {
		h.want(s)
	}

	// The pane follows the chosen row: the report, scrolled to the match.
	h.Key(0, ui.KeyUp)
	h.flush()
	if n := len(h.semd.previewed); n != 2 || h.semd.previewed[1].Path != pdfHit.Path {
		t.Fatalf("previewed %v", h.semd.previewed)
	}
	for _, s := range []string{"Summary", "quarterly revenue", " grew by a fifth.", "Costs held steady.",
		"page 3", "2.4 MB", "12", "abababababababab…", "1 Mar 2026 at 09:30", "40"} {
		h.want(s)
	}
	if h.a.quick.reveal {
		t.Fatal("the match was not scrolled into view")
	}
	h.click("Open report.pdf")
	if !slices.Equal(h.shell.opened, []string{pdfHit.Path}) {
		t.Fatalf("opened %v", h.shell.opened)
	}

	// Clicking the eye of the row shown closes the pane; Cmd-I opens it.
	h.click("Quick Look at report.pdf")
	h.flush()
	if h.a.quick.open || h.HasText("Close Quick Look") {
		t.Fatal("pane still open")
	}
	h.Key(ui.Cmd, ui.KeyI)
	h.flush()
	h.want("Costs held steady.")
	h.Key(ui.Cmd, ui.KeyI)
	h.flush()
	if h.a.quick.open {
		t.Fatal("Cmd-I did not close the pane")
	}
}

// Moving on before a preview comes drops it: the pane shows the last
// chosen, whatever order the replies come in.
func TestQuickLookDropsStale(t *testing.T) {
	h := quickHarness(t)
	h.Key(ui.Cmd, ui.KeyI)
	h.Frame()
	h.Key(0, ui.KeyDown)
	h.Frame()
	if len(h.queue) != 2 {
		t.Fatalf("%d loads queued", len(h.queue))
	}
	h.queue[0], h.queue[1] = h.queue[1], h.queue[0] // the talk answers first
	h.flush()
	if h.a.quick.p.Path != talkHit.Path || h.HasText("Costs held steady.") {
		t.Fatalf("shows %s", h.a.quick.p.Path)
	}

	// New results: the pane follows; none: it says so.
	h.semd.hits["costs"] = []client.Hit{pdfHit}
	h.Key(0, ui.KeyEscape)
	h.Type("costs")
	h.Advance(debounce)
	h.flush()
	h.want("Costs held steady.")
	h.Key(0, ui.KeyEscape)
	h.flush()
	h.want("Choose a result to preview it.")
}

func TestQuickLookErrors(t *testing.T) {
	h := quickHarness(t)
	delete(h.semd.previews, talkHit.Path)
	h.Key(0, ui.KeyDown)
	h.Key(ui.Cmd, ui.KeyI)
	h.flush()
	h.want("Cannot preview: not indexed")

	h.semd.down = errors.New("semd is down")
	h.Key(0, ui.KeyUp)
	h.flush()
	h.want("Cannot preview: semd is down")
}

func TestQuickLookImage(t *testing.T) {
	h := quickHarness(t)
	photo := client.Hit{Path: "/Users/me/Pictures/harbour.png", Kind: "image", Score: .7}
	h.semd.hits["harbour"] = []client.Hit{photo}
	h.semd.previews[photo.Path] = client.Preview{Path: photo.Path, Kind: "image", Width: 40, Height: 30, Size: 120}
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, 40, 30))); err != nil {
		t.Fatal(err)
	}
	h.semd.thumbs = map[string][]byte{photo.Path: b.Bytes()}
	h.Key(0, ui.KeyEscape)
	h.Type("harbour")
	h.Advance(debounce)
	h.flush()
	h.Key(ui.Cmd, ui.KeyI)
	h.flush()
	if h.a.quick.thumb == nil {
		t.Fatalf("no picture: %s", h.a.quick.thumbErr)
	}
	h.want("40 × 30")

	h.semd.thumbs = nil // semd cannot make one: the pane says why
	h.Key(ui.Cmd, ui.KeyI)
	h.Key(ui.Cmd, ui.KeyI)
	h.flush()
	h.want("No picture: no thumbnail")
}

// The pane fits the smallest window: its buttons stay in it.
// SEMSEARCH_SHOT=<abs prefix> saves a picture of it.
func TestQuickLookFitsSmallWindow(t *testing.T) {
	h := quickHarness(t)
	const titleBar = 28 // as in TestSettingsFitsSmallestWindow
	h.SetSize(minWindowW, minWindowH-titleBar)
	h.Key(ui.Cmd, ui.KeyI)
	h.flush()
	if out := os.Getenv("SEMSEARCH_SHOT"); out != "" {
		h.shot(out + "-quicklook.png")
	}
	for _, s := range []string{"Open report.pdf", "Show in Finder", "Close Quick Look", "Quick Look at report.pdf"} {
		r, ok := h.Find(s)
		if !ok || r.X < 0 || r.Y < 0 || r.X+r.W > minWindowW || r.Y+r.H > minWindowH-titleBar {
			t.Errorf("%q at %+v (found %v)", s, r, ok)
		}
	}
}

func TestSplitMatch(t *testing.T) {
	for _, tc := range []struct {
		text, match   string
		before, after string
		lines         []span
	}{
		{"a\nb c d\ne\n", "c", "a", "e", []span{{"b ", false}, {"c", true}, {" d", false}}},
		{"only", "only", "", "", []span{{"only", true}}},
		{"x\nyz", "x\ny", "", "", []span{{"x\ny", true}, {"z", false}}},
		{"one\ntwo\n", "", "one\ntwo", "", nil},
	} {
		s := strings.Index(tc.text, tc.match)
		if tc.match == "" {
			s = 0
		}
		before, lines, after := splitMatch(tc.text, s, s+len(tc.match))
		if before != tc.before || after != tc.after || !slices.Equal(lines, tc.lines) {
			t.Errorf("%q: %q %v %q", tc.text, before, lines, after)
		}
	}
	defer func() {
		if recover() == nil {
			t.Fatal("no panic on a range past the text")
		}
	}()
	splitMatch("abc", 2, 5)
}

func TestByteSize(t *testing.T) {
	for n, want := range map[int64]string{0: "0 bytes", 999: "999 bytes", 1000: "1.0 KB", 2_400_000: "2.4 MB", 5e12: "5.0 TB"} {
		if got := byteSize(n); got != want {
			t.Errorf("byteSize(%d) = %q, want %q", n, got, want)
		}
	}
}

// The eye of another row while the pane is open shows that row.
func TestQuickLookSwitchRow(t *testing.T) {
	h := quickHarness(t)
	h.Key(ui.Cmd, ui.KeyI)
	h.flush()
	h.want("Costs held steady.")
	h.click("Quick Look at keynote.m4a")
	h.flush()
	if !h.a.quick.open || h.a.chosen != 1 || h.a.quick.p.Path != talkHit.Path {
		t.Fatalf("open %v, chosen %d, shows %s", h.a.quick.open, h.a.chosen, h.a.quick.p.Path)
	}
}

package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/msongd/kimquy/internal/client"
	"github.com/msongd/kimquy/internal/kind"
)

// The quick view: a pane beside the results that previews the chosen hit,
// scrolled to the text that matched, with the file's details below. The
// eye on a row or Cmd-I opens and closes it; it follows the chosen row.

const defaultQuickSplit = 52 // percent of the width the results keep

// eyeIcon is the quick view's button on each row, drawn in the text color.
var eyeIcon = ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/></svg>`))

// quickView is the state of the pane.
type quickView struct {
	open  bool
	split float32 // percent of the width the results keep
	key   string  // hitKey of the hit shown or loading; "" for none
	seq   int     // of the latest load; older replies are dropped
	stop  context.CancelFunc
	busy  bool
	p     client.Preview
	err   string
	thumb *ui.Bitmap // an image's picture, decoded
	// thumbErr says why an image shows no picture.
	thumbErr string
	reveal   bool // scroll the match into view in the next frame
}

// toggleQuick opens the quick view on hit i, or closes it if it shows i.
func (a *app) toggleQuick(i int) {
	if i < 0 || i >= len(a.hits) {
		return
	}
	if a.quick.open && i == a.chosen {
		a.closeQuick()
		return
	}
	a.choose(i)
	a.quick.open = true
	if a.quick.split <= 0 {
		a.quick.split = defaultQuickSplit
	}
}

// quickKey is Cmd-I: it opens the quick view on the chosen hit, or
// closes it.
func (a *app) quickKey() {
	if a.quick.open {
		a.closeQuick()
	} else {
		a.toggleQuick(a.chosen)
	}
}

func (a *app) closeQuick() {
	a.quick.open = false
	a.dropPreview()
}

// dropPreview ends the load in flight, if any, and forgets the preview.
func (a *app) dropPreview() {
	a.quick.seq++
	if a.quick.stop != nil {
		a.quick.stop()
		a.quick.stop = nil
	}
	split, open := a.quick.split, a.quick.open
	a.quick = quickView{open: open, split: split, seq: a.quick.seq}
}

// syncQuick loads the preview of the chosen hit when the pane shows
// another (or none): the user moved through the list, or new results came.
func (a *app) syncQuick() {
	if !a.quick.open {
		return
	}
	if len(a.hits) == 0 {
		if a.quick.key != "" {
			a.dropPreview()
		}
		return
	}
	if h := a.hits[a.chosen]; hitKey(h) != a.quick.key {
		a.loadPreview(h)
	}
}

func (a *app) loadPreview(h client.Hit) {
	a.dropPreview()
	seq := a.quick.seq
	a.quick.key, a.quick.busy = hitKey(h), true
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	a.quick.stop = cancel
	a.async(func() func() {
		defer cancel()
		p, err := a.be.Preview(ctx, client.PreviewRequest{Path: h.Path, Locator: h.Locator})
		var (
			thumb    *ui.Bitmap
			thumbErr string
		)
		if err == nil && p.Kind == "image" {
			// Decoded here, off the main thread: a photo takes a while.
			data, terr := a.be.Thumbnail(ctx, h.Path)
			if terr == nil {
				thumb, terr = ui.DecodeBitmap(data)
			}
			if terr != nil {
				thumbErr = "No picture: " + terr.Error()
			}
		}
		return func() {
			if seq != a.quick.seq {
				return // superseded
			}
			a.quick.stop, a.quick.busy = nil, false
			if err != nil {
				a.quick.err = "Cannot preview: " + err.Error()
				return
			}
			a.quick.p, a.quick.thumb, a.quick.thumbErr = p, thumb, thumbErr
			a.quick.reveal = p.MatchEnd > p.MatchStart
		}
	})
}

// quickPane draws the pane: a header, the content scrolled to the match,
// the file's details, and buttons to open it.
func (a *app) quickPane(c *ui.Context) {
	t := c.Theme()
	q := &a.quick
	ui.Column(c).Fill().Children(func() {
		ui.Row(c).Padding(8, 12).Gap(8).AlignItems(ui.Center).Children(func() {
			name := "Quick Look"
			if q.key != "" && len(a.hits) > 0 {
				h := a.hits[a.chosen]
				ui.Badge(c, kindLabel(h.Kind))
				name = filepath.Base(h.Path)
			}
			ui.Text(c, name).Bold().SingleLine().Grow(1).Tooltip(name)
			if q.busy {
				ui.Spinner(c)
			}
			if ui.Button(c, "Close").Label("Close Quick Look").Tooltip("⌘I").Clicked() {
				a.closeQuick()
			}
		})
		ui.Divider(c)
		switch {
		case q.key == "":
			ui.Text(c, "Choose a result to preview it.").TextColor(t.TextMuted).Padding(16).Grow(1)
			return
		case q.err != "":
			ui.Text(c, q.err).TextColor(t.Danger).Padding(16).Wrap().Grow(1)
			return
		case q.busy:
			ui.Column(c).Grow(1)
			return
		}
		ui.Scroll(c).Grow(1).Padding(12).Children(func() { a.quickContent(c) })
		ui.Divider(c)
		a.quickDetails(c)
	})
}

// quickContent shows what the file holds: its text around the match, the
// picture of an image, or what there is to say of the rest.
func (a *app) quickContent(c *ui.Context) {
	t := c.Theme()
	p := a.quick.p
	if p.Stale {
		ui.Text(c, "The file changed since it was indexed: the match may be off.").
			FontSize(11).TextColor(t.Danger).Wrap().Padding(0, 0, 8, 0)
	}
	switch {
	case p.Text != "":
		a.quickText(c, p)
	case p.Kind == "image" && a.quick.thumb != nil:
		ui.Image(c, a.quick.thumb).Fit(ui.ScaleDown).FillWidth().Label(filepath.Base(p.Path))
	case p.Kind == "image":
		ui.Text(c, a.quick.thumbErr).TextColor(t.TextMuted).Wrap()
	case p.TextError != "":
		ui.Text(c, "No text to show: "+p.TextError).TextColor(t.TextMuted).Wrap()
	case p.Kind == "audio" || p.Kind == "video":
		msg := "The match is at " + locatorText(p.Locator) + ". Open plays the file."
		if p.Locator.Kind != kind.LocTime {
			msg = "Open plays the file."
		}
		ui.Text(c, msg).TextColor(t.TextMuted).Wrap()
	default:
		ui.Text(c, "No text to show.").TextColor(t.TextMuted).Wrap()
	}
}

// quickText shows the text in three parts: the lines before the match,
// the lines holding it with the match highlighted, and the lines after.
// The middle part scrolls into view when the preview arrives.
func (a *app) quickText(c *ui.Context, p client.Preview) {
	t := c.Theme()
	font := "system-ui"
	if p.Kind == "text" {
		font = "monospace"
	}
	before, lines, after := splitMatch(p.Text, p.MatchStart, p.MatchEnd)
	ui.Column(c).Selectable().Children(func() {
		if p.TextStart > 0 {
			ui.Text(c, "…").FontSize(11).TextColor(t.TextMuted)
		}
		if before != "" {
			ui.Text(c, before).Font(font).FontSize(12).Wrap()
		}
		if len(lines) > 0 {
			spans := make([]ui.Span, len(lines))
			for i, s := range lines {
				spans[i] = ui.Span{Text: s.text}
				if s.match {
					spans[i].Background, spans[i].Color, spans[i].Radius = t.Accent.Alpha(0.3), t.Text, 3
				}
			}
			m := ui.RichText(c, spans...).Font(font).FontSize(12).Wrap()
			if a.quick.reveal {
				a.quick.reveal = false
				m.ScrollIntoView()
			}
		}
		if after != "" {
			ui.Text(c, after).Font(font).FontSize(12).Wrap()
		}
		if p.TextStart+int64(len(p.Text)) < p.TextTotal {
			ui.Text(c, "…").FontSize(11).TextColor(t.TextMuted)
		}
	})
}

// span is a part of the matching lines, the match or not.
type span struct {
	text  string
	match bool
}

// splitMatch cuts text around [s, end): the whole lines before it, the
// lines holding it as spans, and the whole lines after. Without a match
// everything is before.
func splitMatch(text string, s, end int) (before string, lines []span, after string) {
	if s < 0 || end < s || end > len(text) {
		panic(fmt.Sprintf("splitMatch: [%d,%d) of %d bytes", s, end, len(text)))
	}
	if s == end {
		return strings.TrimSuffix(text, "\n"), nil, ""
	}
	ls := strings.LastIndexByte(text[:s], '\n') + 1
	le := len(text)
	if i := strings.IndexByte(text[end:], '\n'); i >= 0 {
		le = end + i
	}
	before = strings.TrimSuffix(text[:ls], "\n")
	for _, sp := range []span{{text[ls:s], false}, {text[s:end], true}, {text[end:le], false}} {
		if sp.text != "" {
			lines = append(lines, sp)
		}
	}
	after = strings.TrimSuffix(strings.TrimPrefix(text[le:], "\n"), "\n")
	return before, lines, after
}

// quickDetails lists the file's details and the buttons to open it.
func (a *app) quickDetails(c *ui.Context) {
	t := c.Theme()
	p := a.quick.p
	ui.Column(c).Padding(10, 12).Gap(3).Children(func() {
		for _, d := range details(p) {
			ui.Row(c).Key(d[0]).Gap(8).Children(func() {
				ui.Text(c, d[0]).FontSize(11).TextColor(t.TextMuted).Width(76)
				ui.Text(c, d[1]).FontSize(11).SingleLine().Grow(1).Tooltip(d[1])
			})
		}
		ui.Row(c).Gap(8).Padding(6, 0, 0, 0).Children(func() {
			if ui.Button(c, "Open").Label("Open " + filepath.Base(p.Path)).Tooltip("↵").Clicked() {
				a.open(false)
			}
			if ui.Button(c, "Show in Finder").Tooltip("⌘↵").Clicked() {
				a.open(true)
			}
		})
	})
}

// details are the preview's facts, as name and value, for those it has.
func details(p client.Preview) [][2]string {
	out := [][2]string{{"Where", tildePath(filepath.Dir(p.Path))}}
	add := func(name, value string) {
		if value != "" {
			out = append(out, [2]string{name, value})
		}
	}
	add("Match", locatorText(p.Locator))
	add("Size", byteSize(p.Size))
	switch {
	case p.Pages > 0:
		add("Pages", fmt.Sprint(p.Pages))
	case p.Duration > 0:
		add("Duration", clock(p.Duration))
	case p.Width > 0 && p.Height > 0:
		add("Dimensions", fmt.Sprintf("%d × %d", p.Width, p.Height))
	}
	add("Modified", dateTime(p.Modified))
	indexed := dateTime(p.Indexed)
	if indexed != "" && p.Status != "indexed" {
		indexed += " (" + p.Status + ")"
	}
	add("Indexed", indexed)
	add("Fragments", fmt.Sprint(p.Vectors))
	if len(p.SHA256) == 64 {
		add("SHA-256", p.SHA256[:16]+"…")
	}
	return out
}

// byteSize formats n as Finder does, in powers of 1000.
func byteSize(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d bytes", n)
	}
	f, unit := float64(n), ""
	for _, u := range []string{"KB", "MB", "GB", "TB"} {
		f, unit = f/1000, u
		if f < 1000 {
			break
		}
	}
	return fmt.Sprintf("%.1f %s", f, unit)
}

func dateTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2 Jan 2006 at 15:04")
}

// quickButton is the eye on row i that opens or closes the quick view.
func (a *app) quickButton(c *ui.Context, i int, color ui.Color) {
	name := filepath.Base(a.hits[i].Path)
	b := ui.ButtonBase(c).Padding(3).Radius(5).Label("Quick Look at " + name).Tooltip("Quick Look (⌘I)").Children(func() {
		ui.Icon(c, eyeIcon).FontSize(15).TextColor(color)
	})
	if b.Hovered() {
		b.Background(color.Alpha(0.15))
	}
	if b.Clicked() {
		a.toggleQuick(i)
	}
}

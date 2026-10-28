package engine

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // DecodeConfig of previewed images
	_ "image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/msongd/kimquy/internal/catalog"
	"github.com/msongd/kimquy/internal/embed"
	"github.com/msongd/kimquy/internal/kind"
)

const (
	// previewBytes bounds the text a preview carries: the window of the
	// extracted text around the match.
	previewBytes = 64 << 10
	// thumbRawBytes and thumbRawPixels bound an image sent as it is; a
	// bigger one, or one Go cannot decode, is scaled down by sips.
	thumbRawBytes  = 4 << 20
	thumbRawPixels = 2048
	thumbPixels    = 1600 // the longest side of a scaled-down image
)

// previewBudget bounds the time a preview waits for the embedder (a PDF's
// page text, a recording's duration), which during indexing finishes its
// current job first. Past it the preview comes without that part.
var previewBudget = 5 * time.Second

var (
	// ErrNotIndexed: semd previews only the files it indexes.
	ErrNotIndexed = errors.New("not an indexed file")
	// ErrBadPreview is a preview or thumbnail request that cannot be met
	// whatever the state of the file.
	ErrBadPreview = errors.New("bad preview request")
)

// Preview is what the quick view shows of an indexed file: its details,
// and for text, Office and PDF hits the text around the match.
type Preview struct {
	Path     string       `json:"path"`
	Kind     string       `json:"kind"`
	Locator  kind.Locator `json:"locator"`
	Size     int64        `json:"size"`                  // on disk now, or as indexed if it is gone
	Modified time.Time    `json:"modified,omitzero"`     // on disk now; zero if it is gone
	Indexed  time.Time    `json:"indexed,omitzero"`      // the last change of its index status
	Status   string       `json:"status"`                // indexed, pending, failed
	Vectors  int          `json:"vectors"`               // fragments in the index
	SHA256   string       `json:"sha256,omitempty"`      // of the content indexed
	Stale    bool         `json:"stale,omitempty"`       // changed since indexed
	Pages    int          `json:"pages,omitempty"`       // of a PDF
	Duration int64        `json:"duration_ms,omitempty"` // of audio or video
	Width    int          `json:"width,omitempty"`       // of an image, in pixels
	Height   int          `json:"height,omitempty"`
	// Text is a window of the file's extracted text (of the page, for a
	// PDF), cut at line ends where it can be. It starts at TextStart of
	// TextTotal bytes, and [MatchStart, MatchEnd) in it is the hit.
	Text       string `json:"text,omitempty"`
	TextStart  int64  `json:"text_start"`
	TextTotal  int64  `json:"text_total"`
	MatchStart int    `json:"match_start"`
	MatchEnd   int    `json:"match_end"`
	// TextError says why a kind with text has none here.
	TextError string `json:"text_error,omitempty"`
}

// Preview describes the indexed file at path, with its text around loc.
// Only a file it does not index, or a bad locator, is an error: a part it
// cannot read is reported in the preview.
func (e *Engine) Preview(ctx context.Context, path string, loc kind.Locator) (Preview, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return Preview{}, fmt.Errorf("%w: path %q must be absolute and clean", ErrBadPreview, path)
	}
	if err := loc.Validate(); err != nil {
		return Preview{}, fmt.Errorf("%w: %w", ErrBadPreview, err)
	}
	f, ok := e.cat.Lookup(path)
	if !ok {
		return Preview{}, fmt.Errorf("%w: %s", ErrNotIndexed, path)
	}
	if err := locatorFits(f.Kind, loc); err != nil {
		return Preview{}, err
	}
	p := Preview{
		Path: f.Path, Kind: f.Kind.String(), Locator: loc, Size: f.Size,
		Status: f.Status.String(), Vectors: int(f.NVec),
	}
	if f.Updated != 0 {
		p.Indexed = time.Unix(0, f.Updated)
	}
	if f.Hash != ([32]byte{}) {
		p.SHA256 = hex.EncodeToString(f.Hash[:])
	}
	fi, err := os.Stat(f.Path)
	if err != nil {
		p.Stale, p.TextError = true, "cannot read the file: "+err.Error()
		return p, nil
	}
	p.Size, p.Modified = fi.Size(), fi.ModTime()
	p.Stale = fi.Size() != f.Size || fi.ModTime().UnixNano() != f.MTime

	bctx, cancel := context.WithTimeout(ctx, previewBudget)
	defer cancel()
	switch f.Kind {
	case kind.Text, kind.Office:
		cfg := e.Config()
		data, err := readStable(f.Path, cfg.Limit(f.Kind), fi)
		var text string
		if err == nil {
			text, err = e.extractText(f.Path, f.Kind, data)
		}
		if err != nil {
			p.TextError = "cannot read the text: " + err.Error()
			break
		}
		p.setText(text, loc)
	case kind.PDF:
		e.previewPDF(bctx, f, loc, &p)
	case kind.Image:
		p.Width, p.Height = imageSize(f.Path)
	case kind.Audio, kind.Video:
		if e.media == nil {
			break
		}
		if m, err := e.media.ProbeMedia(bctx, embed.Query, f.Path); err == nil {
			p.Duration = m.DurationMS
		} else {
			e.log.Debug("preview probe", "path", f.Path, "err", err)
		}
	}
	return p, ctx.Err()
}

// locatorFits checks that loc can point into a file of kind k.
func locatorFits(k kind.Kind, loc kind.Locator) error {
	ok := loc.Kind == kind.LocNone
	switch k {
	case kind.Text, kind.Office:
		ok = ok || loc.Kind == kind.LocBytes
	case kind.PDF:
		ok = ok || loc.Kind == kind.LocPage
	case kind.Audio, kind.Video:
		ok = ok || loc.Kind == kind.LocTime
	}
	if !ok {
		return fmt.Errorf("%w: locator %s does not fit a %s file", ErrBadPreview, loc, k)
	}
	return nil
}

// previewPDF fills in the page count and the hit's page text.
func (e *Engine) previewPDF(ctx context.Context, f catalog.File, loc kind.Locator, p *Preview) {
	if e.media == nil {
		p.TextError = "this semd reads no PDFs"
		return
	}
	page := max(int(loc.Page), 1)
	res, err := e.media.ExtractPDF(ctx, embed.Query, embed.PDFRequest{Path: f.Path, From: page, Count: 1})
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		p.TextError = "the embedder is busy indexing; try again"
		return
	case err != nil:
		p.TextError = "cannot read the page: " + err.Error()
		return
	}
	p.Pages = res.PageCount
	if len(res.Pages) != 1 {
		p.TextError = fmt.Sprintf("the PDF has no page %d now", page)
		return
	}
	text := res.Pages[0].Text
	if embed.TooLittleText(text) {
		p.TextError = fmt.Sprintf("page %d is a picture: it was searched as an image", page)
		return
	}
	if loc.Kind == kind.LocPage && loc.End == 0 {
		loc = kind.Locator{} // the page as an image: no range in its text
	}
	p.setText(text, loc)
}

// setText keeps the window of text around loc's byte range, which the
// preview's match then points into. A range past the text (the file
// changed) shows the text from its start, without a match.
func (p *Preview) setText(text string, loc kind.Locator) {
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "�")
	}
	s, end := int(loc.Start), int(loc.End)
	if loc.Kind != kind.LocBytes && loc.Kind != kind.LocPage || s >= end || end > len(text) {
		s, end = 0, 0
	}
	lo, hi := window(text, s, end, previewBytes)
	p.Text, p.TextStart, p.TextTotal = text[lo:hi], int64(lo), int64(len(text))
	if end > s {
		p.MatchStart, p.MatchEnd = s-lo, min(end, hi)-lo
	}
}

// window picks at most n bytes of text around [s, end), on rune
// boundaries and, where the room allows, at line ends: the match from its
// start, centered when it is short.
func window(text string, s, end, n int) (lo, hi int) {
	if n <= 0 || s < 0 || end < s || end > len(text) {
		panic(fmt.Sprintf("window: bad range [%d,%d) of %d, n %d", s, end, len(text), n))
	}
	if len(text) <= n {
		return 0, len(text)
	}
	lo = max(0, s-max(0, n-(end-s))/2)
	hi = min(len(text), lo+n)
	lo = max(0, hi-n)
	for lo < s && !utf8.RuneStart(text[lo]) {
		lo++
	}
	for hi > lo && hi < len(text) && !utf8.RuneStart(text[hi]) {
		hi--
	}
	if lo > 0 {
		if i := strings.IndexByte(text[lo:s], '\n'); i >= 0 {
			lo += i + 1
		}
	}
	if hi < len(text) && end < hi {
		if i := strings.LastIndexByte(text[end:hi], '\n'); i >= 0 {
			hi = end + i + 1
		}
	}
	return lo, hi
}

// imageSize reads the pixel size from the header of a PNG, JPEG or GIF;
// zero for other formats.
func imageSize(path string) (w, h int) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	c, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0
	}
	return c.Width, c.Height
}

// Thumbnail returns the indexed image at path for the quick view: as it
// is when it is small and in a format the UI decodes (PNG, JPEG, GIF,
// WebP, BMP), otherwise scaled down to a JPEG by macOS's sips, which reads
// every format semd indexes (HEIC, TIFF…).
func (e *Engine) Thumbnail(ctx context.Context, path string) (data []byte, contentType string, err error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, "", fmt.Errorf("%w: path %q must be absolute and clean", ErrBadPreview, path)
	}
	f, ok := e.cat.Lookup(path)
	if !ok {
		return nil, "", fmt.Errorf("%w: %s", ErrNotIndexed, path)
	}
	if f.Kind != kind.Image {
		return nil, "", fmt.Errorf("%w: %s is not an image", ErrBadPreview, path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, "", err
	}
	ext := strings.ToLower(filepath.Ext(path))
	if t, ok := rawTypes[ext]; ok && fi.Size() <= thumbRawBytes {
		data, err := readStable(path, thumbRawBytes, fi)
		if err != nil {
			return nil, "", err
		}
		c, _, err := image.DecodeConfig(bytes.NewReader(data))
		switch {
		case err == nil && c.Width <= thumbRawPixels && c.Height <= thumbRawPixels:
			return data, t, nil
		case err != nil && (ext == ".webp" || ext == ".bmp") && fi.Size() <= thumbRawBytes/2:
			return data, t, nil // semd cannot read their header (stdlib only); the UI can
		}
	}
	return sipsThumb(ctx, path)
}

var rawTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".bmp": "image/bmp",
}

// sips is macOS's image tool; a var for tests.
var sips = "/usr/bin/sips"

func sipsThumb(ctx context.Context, path string) ([]byte, string, error) {
	dir, err := os.MkdirTemp("", "semd-thumb")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "t.jpg")
	cmd := exec.CommandContext(ctx, sips, "-s", "format", "jpeg", "-s", "formatOptions", "80",
		"-Z", fmt.Sprint(thumbPixels), path, "--out", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, "", fmt.Errorf("sips %s: %w: %s", path, err, strings.TrimSpace(string(b)))
	}
	data, err := os.ReadFile(out)
	if err != nil {
		return nil, "", fmt.Errorf("sips %s: %w", path, err)
	}
	return data, "image/jpeg", nil
}

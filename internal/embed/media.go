package embed

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/msongd/kimquy/internal/kind"
)

// Limits of the extract_pdf op, shared by semd and semd-embed.
const (
	// MaxPDFPagesPerCall bounds one extract_pdf reply. With MaxPDFPageText
	// it keeps a reply well under MaxLine.
	MaxPDFPagesPerCall = 8
	// MaxPDFPageText is the most extracted text one page may carry. A page
	// with more is rejected as invalid rather than cut silently.
	MaxPDFPageText = 512 << 10
	// PDFRenderMinChars: a page with fewer non-space characters of text is
	// rendered and embedded as an image instead (scans, figures, slides).
	PDFRenderMinChars = 32
)

// MediaEmbedder is implemented by embedders that decode media files
// themselves. Paths are absolute; the caller verifies afterwards that the
// file did not change while it was read.
type MediaEmbedder interface {
	// EmbedImage returns one vector for the image file at path.
	EmbedImage(ctx context.Context, p Priority, path string) ([]float32, error)
	// ExtractPDF returns the text of a run of pages, and renders and embeds
	// up to req.Render of them that have too little text.
	ExtractPDF(ctx context.Context, p Priority, req PDFRequest) (PDFPages, error)
	// ProbeMedia reports the duration and tracks of an audio or video file.
	ProbeMedia(ctx context.Context, p Priority, path string) (MediaInfo, error)
	// EmbedAudio returns one vector for a window of the audio, or nil if
	// the window is silent (or past the end of the audio track).
	EmbedAudio(ctx context.Context, p Priority, req AudioRequest) ([]float32, error)
	// EmbedFrames returns one vector per requested time of the video.
	EmbedFrames(ctx context.Context, p Priority, path string, timesMS []int64) ([][]float32, error)
}

// PDFRequest asks for pages [From, From+Count) (1-based) of the PDF at Path.
type PDFRequest struct {
	Path   string
	From   int
	Count  int // 1..MaxPDFPagesPerCall
	Render int // how many text-less pages may be rendered and embedded (0 = none)
}

func (r PDFRequest) validate() error {
	switch {
	case !filepath.IsAbs(r.Path):
		return fmt.Errorf("pdf path %q is not absolute", r.Path)
	case r.From < 1 || r.From > kind.MaxPage:
		return fmt.Errorf("pdf from=%d out of range", r.From)
	case r.Count < 1 || r.Count > MaxPDFPagesPerCall:
		return fmt.Errorf("pdf count=%d, allowed 1..%d", r.Count, MaxPDFPagesPerCall)
	case r.Render < 0 || r.Render > r.Count:
		return fmt.Errorf("pdf render=%d, allowed 0..%d", r.Render, r.Count)
	}
	return nil
}

// PDFPages is one extract_pdf reply.
type PDFPages struct {
	PageCount int // pages in the whole document
	Pages     []PDFPage
}

// PDFPage is the content of one page.
type PDFPage struct {
	Page int
	Text string
	// Vec is the page embedded as an image; set only for pages with too
	// little text, while the request's render budget lasts.
	Vec []float32
	// Unrendered: the page had too little text but the budget was spent.
	Unrendered bool
}

// check verifies a reply against its request; a mismatch is the embedder's bug.
func (r PDFPages) check(req PDFRequest, dims int) error {
	if r.PageCount < 1 || r.PageCount > kind.MaxPage {
		return fmt.Errorf("pdf page_count %d out of 1..%d", r.PageCount, kind.MaxPage)
	}
	want := 0
	if req.From <= r.PageCount {
		want = min(req.Count, r.PageCount-req.From+1)
	}
	if len(r.Pages) != want {
		return fmt.Errorf("pdf: %d pages for from=%d count=%d of %d", len(r.Pages), req.From, req.Count, r.PageCount)
	}
	rendered := 0
	for i, p := range r.Pages {
		switch {
		case p.Page != req.From+i:
			return fmt.Errorf("pdf: page %d at position %d, want %d", p.Page, i, req.From+i)
		case len(p.Text) > MaxPDFPageText:
			return fmt.Errorf("pdf: page %d text is %d bytes, cap %d", p.Page, len(p.Text), MaxPDFPageText)
		case !utf8.ValidString(p.Text):
			return fmt.Errorf("pdf: page %d text is not UTF-8", p.Page)
		case p.Vec != nil && p.Unrendered:
			return fmt.Errorf("pdf: page %d both rendered and unrendered", p.Page)
		case p.Vec != nil && len(p.Vec) != dims:
			return fmt.Errorf("pdf: page %d vector has %d dims, want %d", p.Page, len(p.Vec), dims)
		}
		if p.Vec != nil {
			rendered++
		}
	}
	if rendered > req.Render {
		return fmt.Errorf("pdf: %d pages rendered, budget %d", rendered, req.Render)
	}
	return nil
}

// TooLittleText reports whether a page's text is too short to embed as
// text, so the page should be embedded as an image.
func TooLittleText(s string) bool {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			if n++; n >= PDFRenderMinChars {
				return false
			}
		}
	}
	return true
}

// EmbedImage embeds the words of the file name: a stand-in for vision that
// lets tests find "sunset-beach.jpg" with the query "sunset".
func (f *Fake) EmbedImage(ctx context.Context, _ Priority, path string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return f.vector(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))), nil
}

// ExtractPDF treats the file as UTF-8 text with pages separated by form
// feeds; it is not a PDF parser. Pages with too little text are "rendered"
// by embedding their text together with the file name.
func (f *Fake) ExtractPDF(ctx context.Context, _ Priority, req PDFRequest) (PDFPages, error) {
	if err := req.validate(); err != nil {
		return PDFPages{}, err
	}
	if err := ctx.Err(); err != nil {
		return PDFPages{}, err
	}
	data, err := os.ReadFile(req.Path)
	if err != nil {
		return PDFPages{}, err
	}
	if !utf8.Valid(data) {
		return PDFPages{}, fmt.Errorf("%w: fake pdf is not UTF-8", ErrInvalidInput)
	}
	pages := bytes.Split(data, []byte("\f"))
	if len(pages) > kind.MaxPage {
		return PDFPages{}, fmt.Errorf("%w: %d pages", ErrInvalidInput, len(pages))
	}
	out := PDFPages{PageCount: len(pages)}
	stem := strings.TrimSuffix(filepath.Base(req.Path), filepath.Ext(req.Path))
	budget := req.Render
	for i := req.From; i <= len(pages) && i < req.From+req.Count; i++ {
		p := PDFPage{Page: i, Text: string(pages[i-1])}
		if TooLittleText(p.Text) {
			if budget > 0 {
				budget--
				p.Vec = f.vector(stem + " " + p.Text)
			} else {
				p.Unrendered = true
			}
		}
		out.Pages = append(out.Pages, p)
	}
	return out, nil
}

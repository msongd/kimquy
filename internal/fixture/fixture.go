// Package fixture generates test documents with the standard library only:
// PNG images and PDFs drawn from simple shapes, PDFs with text pages, and
// QuickTime movies of shape scenes with a PCM soundtrack.
// The shapes are crude on purpose; they stand in for photos and scanned
// figures in tests and the retrieval eval.
package fixture

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"strconv"
	"strings"
)

// Shape is one filled figure in unit coordinates: x right, y down, the
// canvas is [0,1]×[0,1].
type Shape struct {
	T string  `json:"t"` // rect, circle, ring, tri, vgrad
	X float64 `json:"x,omitempty"`
	Y float64 `json:"y,omitempty"`
	W float64 `json:"w,omitempty"`
	H float64 `json:"h,omitempty"`
	R float64 `json:"r,omitempty"` // circle/ring radius, relative to the shorter side
	// R2 is a ring's inner radius.
	R2 float64 `json:"r2,omitempty"`
	// Pts are a triangle's corners: x1,y1,x2,y2,x3,y3.
	Pts []float64 `json:"pts,omitempty"`
	C   string    `json:"c"`            // fill, #rrggbb; vgrad: top color
	C2  string    `json:"c2,omitempty"` // vgrad: bottom color
}

// Validate rejects shapes that would draw nothing or garbage.
func (s Shape) Validate() error {
	if _, err := parseColor(s.C); err != nil {
		return err
	}
	switch s.T {
	case "rect":
		if s.W <= 0 || s.H <= 0 {
			return fmt.Errorf("rect needs w, h > 0")
		}
	case "circle":
		if s.R <= 0 {
			return fmt.Errorf("circle needs r > 0")
		}
	case "ring":
		if s.R <= 0 || s.R2 < 0 || s.R2 >= s.R {
			return fmt.Errorf("ring needs 0 ≤ r2 < r")
		}
	case "tri":
		if len(s.Pts) != 6 {
			return fmt.Errorf("tri needs 6 coordinates")
		}
	case "vgrad":
		if _, err := parseColor(s.C2); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown shape %q", s.T)
	}
	return nil
}

func parseColor(s string) (color.RGBA, error) {
	if len(s) != 7 || s[0] != '#' {
		return color.RGBA{}, fmt.Errorf("color %q is not #rrggbb", s)
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return color.RGBA{}, fmt.Errorf("color %q: %w", s, err)
	}
	return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 255}, nil
}

func lerp(a, b uint8, t float64) uint8 {
	return uint8(math.Round(float64(a) + (float64(b)-float64(a))*t))
}

// inside reports whether the unit point (x, y) on a canvas of the given
// aspect (width/height) is covered by s.
func (s Shape) inside(x, y, aspect float64) bool {
	// Radii are relative to the shorter side; measure in those units.
	short := min(1, aspect)
	dx, dy := (x-s.X)*aspect/short, (y-s.Y)/short
	switch s.T {
	case "rect":
		return x >= s.X && x < s.X+s.W && y >= s.Y && y < s.Y+s.H
	case "circle":
		return dx*dx+dy*dy <= s.R*s.R
	case "ring":
		d := dx*dx + dy*dy
		return d <= s.R*s.R && d >= s.R2*s.R2
	case "tri":
		p := s.Pts
		e := func(ax, ay, bx, by float64) float64 { return (bx-ax)*(y-ay) - (by-ay)*(x-ax) }
		a, b, c := e(p[0], p[1], p[2], p[3]), e(p[2], p[3], p[4], p[5]), e(p[4], p[5], p[0], p[1])
		return (a >= 0 && b >= 0 && c >= 0) || (a <= 0 && b <= 0 && c <= 0)
	}
	return false
}

// PNG draws shapes, in order, on a white w×h canvas.
func PNG(path string, w, h int, shapes []Shape) error {
	img, err := draw(w, h, shapes)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}

// draw rasterizes shapes, in order, on a white w×h canvas.
func draw(w, h int, shapes []Shape) (*image.RGBA, error) {
	if w < 1 || h < 1 || w*h > 16<<20 {
		return nil, fmt.Errorf("image size %d×%d out of range", w, h)
	}
	for i, s := range shapes {
		if err := s.Validate(); err != nil {
			return nil, fmt.Errorf("shape %d: %w", i, err)
		}
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	aspect := float64(w) / float64(h)
	for py := range h {
		for px := range w {
			x, y := (float64(px)+.5)/float64(w), (float64(py)+.5)/float64(h)
			c := color.RGBA{255, 255, 255, 255}
			for _, s := range shapes {
				if s.T == "vgrad" {
					a, _ := parseColor(s.C)
					b, _ := parseColor(s.C2)
					c = color.RGBA{lerp(a.R, b.R, y), lerp(a.G, b.G, y), lerp(a.B, b.B, y), 255}
				} else if s.inside(x, y, aspect) {
					c, _ = parseColor(s.C)
				}
			}
			img.SetRGBA(px, py, c)
		}
	}
	return img, nil
}

// Page is one PDF page: text lines (printable ASCII, wrapped to the page)
// or, without text, shapes drawn as vector graphics.
type Page struct {
	Text   string  `json:"text,omitempty"`
	Shapes []Shape `json:"shapes,omitempty"`
}

const (
	pageW, pageH = 612.0, 792.0 // US Letter, points
	margin       = 72.0
	fontSize     = 11.0
	leading      = 14.0
	wrapAt       = 90 // characters per line
)

func wrap(text string) []string {
	var lines []string
	for _, para := range strings.Split(text, "\n") {
		line := ""
		for _, w := range strings.Fields(para) {
			if line != "" && len(line)+1+len(w) > wrapAt {
				lines = append(lines, line)
				line = ""
			}
			if line != "" {
				line += " "
			}
			line += w
		}
		lines = append(lines, line)
	}
	return lines
}

func pdfString(s string) (string, error) {
	var b strings.Builder
	b.WriteByte('(')
	for i := range len(s) {
		c := s[i]
		switch {
		case c == '(' || c == ')' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c >= 0x20 && c < 0x7f:
			b.WriteByte(c)
		default:
			return "", fmt.Errorf("pdf text must be printable ASCII, got byte %#x", c)
		}
	}
	b.WriteByte(')')
	return b.String(), nil
}

func fill(b *strings.Builder, hex string) {
	c, _ := parseColor(hex)
	fmt.Fprintf(b, "%.3f %.3f %.3f rg\n", float64(c.R)/255, float64(c.G)/255, float64(c.B)/255)
}

// shapeOps draws s on the page's drawable area as PDF path operators.
func shapeOps(b *strings.Builder, s Shape) {
	// Unit coordinates (y down) to points (y up) inside the margins.
	w, h := pageW-2*margin, pageH-2*margin
	X := func(x float64) float64 { return margin + x*w }
	Y := func(y float64) float64 { return pageH - margin - y*h }
	circle := func(r float64) {
		// Four Bézier quarter-arcs; k is the standard control-point ratio.
		const k = 0.5523
		cx, cy, rr := X(s.X), Y(s.Y), r*min(w, h)
		fmt.Fprintf(b, "%.2f %.2f m\n", cx+rr, cy)
		fmt.Fprintf(b, "%.2f %.2f %.2f %.2f %.2f %.2f c\n", cx+rr, cy+k*rr, cx+k*rr, cy+rr, cx, cy+rr)
		fmt.Fprintf(b, "%.2f %.2f %.2f %.2f %.2f %.2f c\n", cx-k*rr, cy+rr, cx-rr, cy+k*rr, cx-rr, cy)
		fmt.Fprintf(b, "%.2f %.2f %.2f %.2f %.2f %.2f c\n", cx-rr, cy-k*rr, cx-k*rr, cy-rr, cx, cy-rr)
		fmt.Fprintf(b, "%.2f %.2f %.2f %.2f %.2f %.2f c\n", cx+k*rr, cy-rr, cx+rr, cy-k*rr, cx+rr, cy)
	}
	switch s.T {
	case "rect":
		fill(b, s.C)
		fmt.Fprintf(b, "%.2f %.2f %.2f %.2f re f\n", X(s.X), Y(s.Y+s.H), s.W*w, s.H*h)
	case "circle":
		fill(b, s.C)
		circle(s.R)
		b.WriteString("f\n")
	case "ring":
		fill(b, s.C)
		circle(s.R)
		if s.R2 > 0 {
			circle(s.R2)
		}
		b.WriteString("f*\n") // even-odd: the inner circle is a hole
	case "tri":
		fill(b, s.C)
		p := s.Pts
		fmt.Fprintf(b, "%.2f %.2f m %.2f %.2f l %.2f %.2f l h f\n", X(p[0]), Y(p[1]), X(p[2]), Y(p[3]), X(p[4]), Y(p[5]))
	case "vgrad":
		a, _ := parseColor(s.C)
		c, _ := parseColor(s.C2)
		const bands = 64
		for i := range bands {
			t := (float64(i) + .5) / bands
			fmt.Fprintf(b, "%.3f %.3f %.3f rg\n", float64(lerp(a.R, c.R, t))/255, float64(lerp(a.G, c.G, t))/255, float64(lerp(a.B, c.B, t))/255)
			fmt.Fprintf(b, "%.2f %.2f %.2f %.2f re f\n", X(0), Y(float64(i+1)/bands), w, h/bands+0.5)
		}
	}
}

// PDF writes a minimal PDF 1.4 file: Helvetica text pages and vector
// shape pages. Text that does not fit on one page is an error.
func PDF(path string, pages []Page) error {
	if len(pages) == 0 {
		return errors.New("pdf needs at least one page")
	}
	maxLines := int(math.Floor((pageH - 2*margin) / leading))
	var contents []string
	for i, p := range pages {
		var b strings.Builder
		if p.Text != "" {
			lines := wrap(p.Text)
			if len(lines) > maxLines {
				return fmt.Errorf("page %d: %d lines, at most %d fit", i+1, len(lines), maxLines)
			}
			fmt.Fprintf(&b, "BT /F1 %g Tf %g TL %g %g Td\n", fontSize, leading, margin, pageH-margin)
			for _, l := range lines {
				s, err := pdfString(l)
				if err != nil {
					return fmt.Errorf("page %d: %w", i+1, err)
				}
				b.WriteString(s + " Tj T*\n")
			}
			b.WriteString("ET\n")
		}
		for j, s := range p.Shapes {
			if err := s.Validate(); err != nil {
				return fmt.Errorf("page %d shape %d: %w", i+1, j, err)
			}
			shapeOps(&b, s)
		}
		contents = append(contents, b.String())
	}

	// Objects: 1 catalog, 2 page tree, 3 font, then page and content pairs.
	var objs []string
	kids := make([]string, len(pages))
	for i := range pages {
		kids[i] = fmt.Sprintf("%d 0 R", 4+2*i)
	}
	objs = append(objs,
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	for i, c := range contents {
		objs = append(objs,
			fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %g %g] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", pageW, pageH, 5+2*i),
			fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(c), c))
	}
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return os.WriteFile(path, out.Bytes(), 0o600)
}

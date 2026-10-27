package embed

import (
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msongd/kimquy/internal/fixture"
	"github.com/msongd/kimquy/internal/kind"
)

// Shapes shared by the image and PDF checks.
var (
	redDisc  = []fixture.Shape{{T: "circle", X: .5, Y: .5, R: .3, C: "#bc002d"}}
	tricolor = []fixture.Shape{
		{T: "rect", X: 0, Y: 0, W: 1. / 3, H: 1, C: "#0055a4"},
		{T: "rect", X: 2. / 3, Y: 0, W: 1. / 3, H: 1, C: "#ef4135"},
	}
	sunset = []fixture.Shape{
		{T: "vgrad", C: "#ff9a3c", C2: "#5b1e5f"},
		{T: "circle", X: .5, Y: .62, R: .14, C: "#ffd23f"},
		{T: "rect", X: 0, Y: .62, W: 1, H: .38, C: "#1b1b2f"},
	}
	checker = func() []fixture.Shape {
		var s []fixture.Shape
		for y := range 8 {
			for x := range 8 {
				if (x+y)%2 == 0 {
					s = append(s, fixture.Shape{T: "rect", X: float64(x) / 8, Y: float64(y) / 8, W: 1. / 8, H: 1. / 8, C: "#000000"})
				}
			}
		}
		return s
	}()
	target = []fixture.Shape{
		{T: "circle", X: .5, Y: .5, R: .45, C: "#d62828"},
		{T: "circle", X: .5, Y: .5, R: .33, C: "#ffffff"},
		{T: "circle", X: .5, Y: .5, R: .21, C: "#d62828"},
		{T: "circle", X: .5, Y: .5, R: .09, C: "#ffffff"},
	}
)

func norm(v []float32) float64 { return math.Sqrt(dot(v, v)) }

// A red disc on a transparent background: must embed like the opaque one.
func transparentDisc(t *testing.T, path string) {
	img := image.NewNRGBA(image.Rect(0, 0, 600, 400))
	for y := range 400 {
		for x := range 600 {
			dx, dy := float64(x-300)/400, float64(y-200)/400
			if dx*dx+dy*dy <= .3*.3 {
				img.SetNRGBA(x, y, color.NRGBA{0xbc, 0, 0x2d, 255})
			}
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestModelImage(t *testing.T) {
	s := startModel(t)
	if !s.Info().Kinds.Has(kind.Image) || !s.Info().Kinds.Has(kind.PDF) {
		t.Fatalf("sidecar offers no images: %+v", s.Info())
	}
	ctx := context.Background()
	dir := t.TempDir()
	images := []struct {
		name   string
		shapes []fixture.Shape
		query  string
	}{
		{"disc.png", redDisc, "a red circle on a white background"},
		{"tricolor.png", tricolor, "vertical stripes of blue, white and red"},
		{"sunset.png", sunset, "the sun setting over the horizon"},
		{"checker.png", checker, "a black and white checkerboard pattern"},
		{"target.png", target, "a red and white archery target"},
	}
	vecs := make([][]float32, len(images))
	for i, im := range images {
		p := filepath.Join(dir, im.name)
		if err := fixture.PNG(p, 900, 600, im.shapes); err != nil {
			t.Fatal(err)
		}
		v, err := s.EmbedImage(ctx, Index, p)
		if err != nil {
			t.Fatalf("%s: %v", im.name, err)
		}
		if n := norm(v); math.Abs(n-1) > 1e-3 {
			t.Fatalf("%s: norm %.4f", im.name, n)
		}
		vecs[i] = v
	}
	correct := 0
	for i, im := range images {
		q, err := s.EmbedTexts(ctx, Query, []string{QueryPrompt(im.query)})
		if err != nil {
			t.Fatal(err)
		}
		best := 0
		for j := range vecs {
			if dot(q[0], vecs[j]) > dot(q[0], vecs[best]) {
				best = j
			}
		}
		t.Logf("%-45q → %-13s %.3f (want %s %.3f)", im.query, images[best].name, dot(q[0], vecs[best]), im.name, dot(q[0], vecs[i]))
		if best == i {
			correct++
		}
	}
	t.Logf("text→image top-1: %d/%d", correct, len(images))
	if correct < len(images)-1 {
		t.Errorf("text→image top-1 %d/%d", correct, len(images))
	}

	alpha := filepath.Join(dir, "alpha.png")
	transparentDisc(t, alpha)
	v, err := s.EmbedImage(ctx, Index, alpha)
	if err != nil {
		t.Fatalf("transparent png: %v", err)
	}
	if c := dot(v, vecs[0]); c < 0.9 {
		t.Errorf("transparent disc vs opaque disc cosine %.3f, want ≥ .9", c)
	}

	junk := filepath.Join(dir, "junk.jpg")
	if err := os.WriteFile(junk, []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EmbedImage(ctx, Index, junk); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("junk image: err %v, want ErrInvalidInput", err)
	}
}

func TestModelPDF(t *testing.T) {
	s := startModel(t)
	if !s.Info().Kinds.Has(kind.PDF) {
		t.Fatalf("sidecar offers no PDFs: %+v", s.Info())
	}
	ctx := context.Background()
	dir := t.TempDir()
	p := filepath.Join(dir, "a.pdf")
	glaciers := "Glaciers carve U-shaped valleys as they slowly move downhill, scraping bedrock and carrying boulders far from their source."
	err := fixture.PDF(p, []fixture.Page{
		{Text: glaciers},
		{Shapes: target},
		{}, // blank: over the render budget below
		{Text: "Volcanic islands form where magma rises through the ocean floor."},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.ExtractPDF(ctx, Index, PDFRequest{Path: p, From: 1, Count: 8, Render: 1})
	if err != nil {
		t.Fatal(err)
	}
	if r.PageCount != 4 || len(r.Pages) != 4 {
		t.Fatalf("pages: %+v", r)
	}
	if got := strings.Join(strings.Fields(r.Pages[0].Text), " "); got != glaciers {
		t.Errorf("page 1 text %q", got)
	}
	if r.Pages[1].Vec == nil || r.Pages[2].Vec != nil || !r.Pages[2].Unrendered || r.Pages[3].Vec != nil {
		t.Fatalf("render decisions wrong: %+v", r.Pages)
	}
	// The rendered page should look like the same drawing as a PNG.
	png := filepath.Join(dir, "target.png")
	if err := fixture.PNG(png, 612, 792, target); err != nil {
		t.Fatal(err)
	}
	iv, err := s.EmbedImage(ctx, Index, png)
	if err != nil {
		t.Fatal(err)
	}
	c := dot(r.Pages[1].Vec, iv)
	t.Logf("rendered page vs png of the same shapes: cosine %.3f", c)
	if c < 0.8 {
		t.Errorf("rendered page vs png cosine %.3f, want ≥ .8", c)
	}

	// A later run of pages, as semd reads it.
	r, err = s.ExtractPDF(ctx, Query, PDFRequest{Path: p, From: 4, Count: 1})
	if err != nil || len(r.Pages) != 1 || !strings.Contains(r.Pages[0].Text, "Volcanic") {
		t.Fatalf("page 4: %+v, %v", r, err)
	}
	r, err = s.ExtractPDF(ctx, Query, PDFRequest{Path: p, From: 5, Count: 1})
	if err != nil || r.PageCount != 4 || len(r.Pages) != 0 {
		t.Fatalf("past the end: %+v, %v", r, err)
	}

	junk := filepath.Join(dir, "junk.pdf")
	if err := os.WriteFile(junk, []byte("%PDF-1.4 truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExtractPDF(ctx, Index, PDFRequest{Path: junk, From: 1, Count: 1}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("junk pdf: err %v, want ErrInvalidInput", err)
	}
}

// BenchmarkModelImage: one photo-sized image, decode through embedding.
func BenchmarkModelImage(b *testing.B) {
	s := startModel(b)
	ctx := context.Background()
	p := filepath.Join(b.TempDir(), "sunset.png")
	if err := fixture.PNG(p, 4032, 3024, sunset); err != nil { // a 12 MP phone photo
		b.Fatal(err)
	}
	if _, err := s.EmbedImage(ctx, Index, p); err != nil { // warm-up: vision load, kernels
		b.Fatal(err)
	}
	for b.Loop() {
		if _, err := s.EmbedImage(ctx, Index, p); err != nil {
			b.Fatal(err)
		}
	}
}

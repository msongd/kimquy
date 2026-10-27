// Command icon draws the Kim Quy app icon: the Golden Turtle seen from
// above, rising through the deep water of the lake, with the ripples of a
// search around it. It writes an .iconset folder for iconutil:
//
//	go run ./packaging/icon -out build/AppIcon.iconset
//	iconutil -c icns build/AppIcon.iconset
//
// Every size is drawn, not scaled: shapes are signed distance fields in a
// 1024-unit design space, so edges stay sharp and anti-aliased at 16 px.
package main

import (
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
)

func main() {
	log.SetFlags(0)
	out := flag.String("out", "", "the .iconset directory to write (created)")
	flag.Parse()
	if *out == "" || filepath.Ext(*out) != ".iconset" || flag.NArg() != 0 {
		log.Fatal("usage: icon -out <dir>.iconset")
	}
	if err := write(*out); err != nil {
		log.Fatal(err)
	}
}

// write renders the sizes iconutil wants into dir.
func write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, s := range []int{16, 32, 128, 256, 512} {
		for _, scale := range []int{1, 2} {
			name := fmt.Sprintf("icon_%dx%d.png", s, s)
			if scale == 2 {
				name = fmt.Sprintf("icon_%dx%d@2x.png", s, s)
			}
			if err := save(filepath.Join(dir, name), render(s*scale)); err != nil {
				return err
			}
		}
	}
	return nil
}

func save(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// rgba is a premultiplied color with components in [0, 1].
type rgba struct{ r, g, b, a float64 }

func hex(v uint32) rgba {
	return rgba{float64(v>>16&255) / 255, float64(v>>8&255) / 255, float64(v&255) / 255, 1}
}

// with is c at alpha a (c opaque).
func (c rgba) with(a float64) rgba { return rgba{c.r * a, c.g * a, c.b * a, a} }

// over composites src over dst.
func over(dst, src rgba) rgba {
	k := 1 - src.a
	return rgba{src.r + dst.r*k, src.g + dst.g*k, src.b + dst.b*k, src.a + dst.a*k}
}

func mix(a, b rgba, t float64) rgba {
	t = clamp(t, 0, 1)
	return rgba{a.r + (b.r-a.r)*t, a.g + (b.g-a.g)*t, a.b + (b.b-a.b)*t, a.a + (b.a-a.a)*t}
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

func smoothstep(e0, e1, x float64) float64 {
	t := clamp((x-e0)/(e1-e0), 0, 1)
	return t * t * (3 - 2*t)
}

// Shapes: signed distances in design units, negative inside.

func circle(x, y, cx, cy, r float64) float64 { return math.Hypot(x-cx, y-cy) - r }

// ellipse approximates the distance to an ellipse of radii rx, ry centered
// at cx, cy and turned by angle (radians); good near its edge.
func ellipse(x, y, cx, cy, rx, ry, angle float64) float64 {
	x, y = x-cx, y-cy
	s, c := math.Sincos(-angle)
	x, y = x*c-y*s, x*s+y*c
	k0 := math.Hypot(x/rx, y/ry)
	k1 := math.Hypot(x/(rx*rx), y/(ry*ry))
	if k1 == 0 {
		return -math.Min(rx, ry)
	}
	return k0 * (k0 - 1) / k1
}

// squircle approximates the distance to the superellipse of macOS icons.
func squircle(x, y, half float64) float64 {
	const n = 5
	return math.Pow(math.Pow(math.Abs(x), n)+math.Pow(math.Abs(y), n), 1.0/n) - half
}

// segment is the distance to the segment from a to b.
func segment(x, y, ax, ay, bx, by float64) float64 {
	px, py, dx, dy := x-ax, y-ay, bx-ax, by-ay
	t := clamp((px*dx+py*dy)/(dx*dx+dy*dy), 0, 1)
	return math.Hypot(px-dx*t, py-dy*t)
}

// The design, in units of a 1024 canvas centered on 0, 0 (y down).
const (
	canvas   = 1024
	tileHalf = 412 // the tile: 824 wide, as Apple's template
	shellX   = 0
	shellY   = 30
	shellRX  = 186
	shellRY  = 214
	rim      = 24 // the rim of the shell, inward from its edge
)

var (
	deepTop    = hex(0x0f6e7a)
	deepBottom = hex(0x05253a)
	glow       = hex(0x3fc1b5)
	gold       = hex(0xf0b93c)
	goldLight  = hex(0xffe39a)
	goldDark   = hex(0xb47716)
	goldEdge   = hex(0x7d4c0c)
	ink        = hex(0x2b1a05)
	white      = hex(0xffffff)
	black      = hex(0x000000)
)

// The scutes: a hexagon in the middle of the shell, and the seams from its
// corners to the rim.
var hexCorners = func() [6][2]float64 {
	var v [6][2]float64
	for i := range v {
		a := math.Pi/2 + float64(i)*math.Pi/3
		v[i] = [2]float64{shellX + 84*math.Cos(a), shellY - 4 + 96*math.Sin(a)}
	}
	return v
}()

// seams is the distance to the lines on the shell.
func seams(x, y float64) float64 {
	d := math.Inf(1)
	for i, a := range hexCorners {
		b := hexCorners[(i+1)%6]
		d = math.Min(d, segment(x, y, a[0], a[1], b[0], b[1]))
		// Outward from the corner, past the rim (clipped by the shell).
		dx, dy := a[0]-shellX, a[1]-(shellY-4)
		d = math.Min(d, segment(x, y, a[0], a[1], a[0]+dx*2, a[1]+dy*2))
	}
	// The ring of marginal scutes.
	return math.Min(d, math.Abs(ellipse(x, y, shellX, shellY, shellRX-62, shellRY-62, 0)))
}

// body is the distance to the head, flippers and tail (under the shell).
func body(x, y float64) float64 {
	ax := math.Abs(x)                                         // the turtle is symmetric
	d := ellipse(ax, y, 0, -262, 58, 72, 0)                   // head
	d = math.Min(d, ellipse(ax, y, 0, -200, 40, 60, 0))       // neck
	d = math.Min(d, ellipse(ax, y, 182, -92, 122, 44, -0.62)) // front flippers
	d = math.Min(d, ellipse(ax, y, 146, 214, 74, 34, 0.68))   // back flippers
	d = math.Min(d, ellipse(ax, y, 0, 268, 20, 44, 0))        // tail
	return d
}

func shell(x, y float64) float64 { return ellipse(x, y, shellX, shellY, shellRX, shellRY, 0) }

// shade is the color at x, y; px is the size of an output pixel in design
// units, the width of anti-aliased edges.
func shade(x, y, px float64) rgba {
	cover := func(d float64) float64 { return clamp(0.5-d/px, 0, 1) }
	var c rgba

	// The tile's shadow, then the tile: deep water, lighter where the
	// turtle rises, with ripples spreading from it.
	c = over(c, black.with(0.32*(1-smoothstep(-20, 30, squircle(x, y-14, tileHalf)))))
	tile := cover(squircle(x, y, tileHalf))
	if tile > 0 {
		w := mix(deepTop, deepBottom, (y+tileHalf)/(2*tileHalf))
		g := 1 - smoothstep(0, 470, math.Hypot(x, (y-20)*1.05))
		w = over(w, glow.with(0.42*g*g))
		for i, r := range []float64{318, 382, 452} {
			ring := math.Abs(circle(x, y, 0, 20, r)) - math.Max(5-float64(i), px/2)
			w = over(w, white.with((0.13-0.035*float64(i))*cover(ring)))
		}
		// Light from the surface, a soft band across the top.
		w = over(w, white.with(0.07*(1-smoothstep(-tileHalf, -120, y))))
		c = over(c, w.with(tile))
	}

	// The turtle's shadow on the water.
	sd := math.Min(body(x-10, y-22), shell(x-10, y-22))
	c = over(c, black.with(0.3*(1-smoothstep(-30, 40, sd))))

	// Head, flippers and tail: gold, darker at the edges and to the back.
	if b := body(x, y); b < px {
		k := mix(gold, goldDark, smoothstep(-260, 300, y)*0.6)
		k = mix(k, goldEdge, smoothstep(-16, 0, b))
		c = over(c, k.with(cover(b)))
		// Eyes.
		e := math.Min(circle(math.Abs(x), y, 27, -280, 10), 1e9)
		c = over(c, ink.with(cover(e)))
	}

	// The shell: lit from the upper left, a rim, and its seams.
	if s := shell(x, y); s < px {
		l := math.Hypot(x+70, y+60) / 330
		k := mix(goldLight, gold, smoothstep(0, 0.55, l))
		k = mix(k, goldDark, smoothstep(0.55, 1.15, l))
		k = mix(k, goldDark, smoothstep(-rim-2, -rim+2, s)*0.85) // the rim
		k = mix(k, goldEdge, smoothstep(-8, 0, s))
		inner := cover(s + rim)
		line := cover(seams(x, y) - math.Max(5.5, px*0.6))
		k = mix(k, goldEdge, line*inner*0.9)
		// A glint, as on polished bronze.
		gl := ellipse(x, y, -70, -70, 70, 40, -0.6)
		k = over(k, white.with(0.38*(1-smoothstep(-40, 30, gl))*inner))
		c = over(c, k.with(cover(s)))
	}
	return c
}

// render draws the icon at size pixels square.
func render(size int) *image.NRGBA {
	if size <= 0 {
		panic(errors.New("icon: size must be positive"))
	}
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	px := float64(canvas) / float64(size)
	// Small sizes take 4×4 samples a pixel: the fine lines would alias.
	n := 1
	if size < 128 {
		n = 4
	}
	for j := range size {
		for i := range size {
			var sum rgba
			for sj := range n {
				for si := range n {
					x := (float64(i)+(float64(si)+0.5)/float64(n))*px - canvas/2
					y := (float64(j)+(float64(sj)+0.5)/float64(n))*px - canvas/2
					c := shade(x, y, px/float64(n))
					sum = rgba{sum.r + c.r, sum.g + c.g, sum.b + c.b, sum.a + c.a}
				}
			}
			k := float64(n * n)
			a := sum.a / k
			if a <= 0 {
				continue
			}
			img.SetNRGBA(i, j, color.NRGBA{
				R: uint8(clamp(sum.r/k/a, 0, 1)*255 + 0.5),
				G: uint8(clamp(sum.g/k/a, 0, 1)*255 + 0.5),
				B: uint8(clamp(sum.b/k/a, 0, 1)*255 + 0.5),
				A: uint8(clamp(a, 0, 1)*255 + 0.5),
			})
		}
	}
	return img
}

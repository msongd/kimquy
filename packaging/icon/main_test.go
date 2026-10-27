package main

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestRender(t *testing.T) {
	for _, size := range []int{16, 64, 256} {
		img := render(size)
		if b := img.Bounds(); b.Dx() != size || b.Dy() != size {
			t.Fatalf("%d: bounds %v", size, b)
		}
		// Outside the tile is clear; the shell in the middle is opaque gold.
		if a := img.NRGBAAt(0, 0).A; a != 0 {
			t.Errorf("%d: corner alpha %d", size, a)
		}
		c := img.NRGBAAt(size/2, size/2+size*40/1024)
		if c.A != 255 || c.R < 200 || c.B > 150 {
			t.Errorf("%d: middle %v, want opaque gold", size, c)
		}
	}
}

func TestWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "AppIcon.iconset")
	if err := write(dir); err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int{
		"icon_16x16.png": 16, "icon_16x16@2x.png": 32, "icon_32x32.png": 32, "icon_32x32@2x.png": 64,
		"icon_128x128.png": 128, "icon_128x128@2x.png": 256, "icon_256x256.png": 256,
		"icon_256x256@2x.png": 512, "icon_512x512.png": 512, "icon_512x512@2x.png": 1024,
	} {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := png.DecodeConfig(f)
		f.Close()
		if err != nil || cfg.Width != size || cfg.Height != size {
			t.Errorf("%s: %dx%d, %v; want %d", name, cfg.Width, cfg.Height, err, size)
		}
	}
	if es, _ := os.ReadDir(dir); len(es) != 10 {
		t.Errorf("%d files, want 10", len(es))
	}
}

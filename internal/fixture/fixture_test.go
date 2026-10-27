package fixture

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

func TestPNG(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.png")
	err := PNG(p, 40, 20, []Shape{{T: "rect", X: 0, Y: 0, W: .5, H: 1, C: "#ff0000"}, {T: "circle", X: .75, Y: .5, R: .25, C: "#0000ff"}})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := os.ReadFile(p)
	img, err := png.Decode(bytes.NewReader(f))
	if err != nil {
		t.Fatal(err)
	}
	if r, g, b, _ := img.At(5, 10).RGBA(); r>>8 != 255 || g != 0 || b != 0 {
		t.Errorf("rect pixel %d %d %d", r, g, b)
	}
	if r, _, b, _ := img.At(30, 10).RGBA(); r != 0 || b>>8 != 255 {
		t.Errorf("circle pixel")
	}
	if r, g, b, _ := img.At(39, 0).RGBA(); r>>8 != 255 || g>>8 != 255 || b>>8 != 255 {
		t.Errorf("corner should be white")
	}
	if PNG(p, 10, 10, []Shape{{T: "blob", C: "#000000"}}) == nil || PNG(p, 10, 10, []Shape{{T: "rect", W: 1, H: 1, C: "red"}}) == nil {
		t.Error("bad shapes accepted")
	}
}

// The cross-reference table must point at each object exactly.
func TestPDFXref(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.pdf")
	err := PDF(p, []Page{{Text: "Hello (world) \\ back"}, {Shapes: []Shape{{T: "ring", X: .5, Y: .5, R: .3, R2: .1, C: "#cc0000"}}}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	m := regexp.MustCompile(`startxref\n(\d+)\n`).FindSubmatch(b)
	if m == nil {
		t.Fatal("no startxref")
	}
	x, _ := strconv.Atoi(string(m[1]))
	if !bytes.HasPrefix(b[x:], []byte("xref\n0 8\n")) {
		t.Fatalf("startxref %d does not point at the table", x)
	}
	for i, e := range regexp.MustCompile(`(\d{10}) 00000 n`).FindAllSubmatch(b[x:], -1) {
		off, _ := strconv.Atoi(string(e[1]))
		if want := strconv.Itoa(i+1) + " 0 obj"; !bytes.HasPrefix(b[off:], []byte(want)) {
			t.Fatalf("object %d offset %d wrong", i+1, off)
		}
	}
	if PDF(p, []Page{{Text: "café"}}) == nil {
		t.Error("non-ASCII text accepted")
	}
}

// walk lists the atoms in b, descending into containers.
func walk(t *testing.T, b []byte, depth string, out map[string][][]byte) {
	t.Helper()
	for off := 0; off < len(b); {
		if len(b)-off < 8 {
			t.Fatalf("%s: %d stray bytes", depth, len(b)-off)
		}
		n := int(binary.BigEndian.Uint32(b[off:]))
		if n < 8 || n > len(b)-off {
			t.Fatalf("%s: atom at %d claims %d bytes of %d", depth, off, n, len(b)-off)
		}
		typ := string(b[off+4 : off+8])
		path := depth + "/" + typ
		out[path] = append(out[path], b[off+8:off+n])
		switch typ {
		case "moov", "trak", "mdia", "minf", "stbl", "dinf":
			walk(t, b[off+8:off+n], path, out)
		}
		off += n
	}
}

func TestMOV(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.mov")
	audio := []int16{1, -2, 3, 0x7fff, -0x8000}
	scenes := []Scene{{MS: 2000, Shapes: []Shape{{T: "rect", W: 1, H: 1, C: "#ff0000"}}}, {MS: 3000}}
	if err := MOV(p, 64, 48, scenes, audio); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	atoms := map[string][][]byte{}
	walk(t, b, "", atoms)
	stco := atoms["/moov/trak/mdia/minf/stbl/stco"]
	if len(stco) != 2 || len(atoms["/mdat"]) != 1 {
		t.Fatalf("%d stco, %d mdat", len(stco), len(atoms["/mdat"]))
	}
	// Video chunks start with a JPEG SOI; the audio chunk holds the samples.
	video, sound := stco[0], stco[1]
	if binary.BigEndian.Uint32(video[4:]) != 2 {
		t.Fatalf("video stco %x", video)
	}
	for i := range 2 {
		off := binary.BigEndian.Uint32(video[8+4*i:])
		if !bytes.HasPrefix(b[off:], []byte{0xff, 0xd8, 0xff}) {
			t.Errorf("frame %d at %d is not a JPEG", i, off)
		}
	}
	off := binary.BigEndian.Uint32(sound[8:])
	for i, want := range audio {
		if got := int16(binary.LittleEndian.Uint16(b[int(off)+2*i:])); got != want {
			t.Errorf("sample %d = %d, want %d", i, got, want)
		}
	}
	mvhd := atoms["/moov/mvhd"][0]
	if d := binary.BigEndian.Uint32(mvhd[16:]); d != 5000 {
		t.Errorf("movie lasts %d ms, want 5000", d)
	}
	if MOV(p, 10, 10, nil, nil) == nil || MOV(p, 10, 10, []Scene{{MS: 0}}, nil) == nil {
		t.Error("bad scenes accepted")
	}
}

func TestReadWAV(t *testing.T) {
	le := binary.LittleEndian
	wav := func(rate uint32, samples ...int16) []byte {
		var data []byte
		for _, s := range samples {
			data = le.AppendUint16(data, uint16(s))
		}
		fmtc := le.AppendUint16(nil, 1)
		fmtc = le.AppendUint16(fmtc, 1)
		fmtc = le.AppendUint32(fmtc, rate)
		fmtc = le.AppendUint32(fmtc, rate*2)
		fmtc = le.AppendUint16(fmtc, 2)
		fmtc = le.AppendUint16(fmtc, 16)
		var b []byte
		b = append(b, "WAVE"...)
		b = append(b, "fmt "...)
		b = le.AppendUint32(b, uint32(len(fmtc)))
		b = append(b, fmtc...)
		b = append(b, "data"...)
		b = le.AppendUint32(b, uint32(len(data)))
		b = append(b, data...)
		return append(le.AppendUint32([]byte("RIFF"), uint32(len(b))), b...)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "a.wav")
	os.WriteFile(p, wav(16000, 5, -7, 300), 0o600)
	got, err := ReadWAV(p)
	if err != nil || len(got) != 3 || got[0] != 5 || got[1] != -7 || got[2] != 300 {
		t.Fatalf("%v %v", got, err)
	}
	os.WriteFile(p, wav(44100, 1), 0o600)
	if _, err := ReadWAV(p); err == nil {
		t.Error("44.1 kHz accepted")
	}
	os.WriteFile(p, wav(16000, 1)[:30], 0o600)
	if _, err := ReadWAV(p); err == nil {
		t.Error("truncated WAV accepted")
	}
}

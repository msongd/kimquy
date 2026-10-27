package catalog

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/msongd/kimquy/internal/kind"
)

func mustOpen(t testing.TB, dir string, opt Options) *Catalog {
	t.Helper()
	c, err := Open(dir, opt)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return c
}

func file(path string) *File {
	return &File{Path: path, Kind: kind.Text, Status: Pending, Size: 10, MTime: 42}
}

func TestPutGetReopen(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir, Options{})
	a, b := file("/a.txt"), file("/b.txt")
	if err := c.Put(a, b); err != nil {
		t.Fatal(err)
	}
	if a.ID == 0 || b.ID == 0 || a.ID == b.ID {
		t.Fatalf("ids not assigned: %d %d", a.ID, b.ID)
	}
	a.Status, a.Gen, a.NVec, a.Hash[0] = Indexed, 7, 3, 0xab
	if err := c.Put(a); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(b.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.SetMeta("fsevents.id", "12345"); err != nil {
		t.Fatal(err)
	}
	c.Close()

	c = mustOpen(t, dir, Options{})
	defer c.Close()
	got, ok := c.Lookup("/a.txt")
	if !ok || got != *a {
		t.Fatalf("Lookup = %+v, %v; want %+v", got, ok, *a)
	}
	if _, ok := c.Lookup("/b.txt"); ok {
		t.Fatal("deleted file came back")
	}
	if v, _ := c.Meta("fsevents.id"); v != "12345" {
		t.Fatalf("meta = %q", v)
	}
	if lg := c.LiveGens(); len(lg) != 1 || lg[a.ID] != 7 {
		t.Fatalf("LiveGens = %v", lg)
	}
	// New IDs never reuse deleted ones.
	d := file("/d.txt")
	if err := c.Put(d); err != nil || d.ID <= b.ID {
		t.Fatalf("new id %d after deleted %d (err %v)", d.ID, b.ID, err)
	}
}

func TestValidation(t *testing.T) {
	c := mustOpen(t, t.TempDir(), Options{})
	defer c.Close()
	for _, f := range []*File{
		{Path: "rel.txt", Kind: kind.Text, Status: Pending},
		{Path: "/a/../b", Kind: kind.Text, Status: Pending},
		{Path: "/x", Kind: kind.Unknown, Status: Pending},
		{Path: "/x", Kind: kind.Text, Status: 0},
		{Path: "/x", Kind: kind.Text, Status: Pending, Size: -1},
	} {
		if err := c.Put(f); err == nil {
			t.Errorf("Put(%+v) succeeded", f)
		}
	}
	if err := c.Put(file("/dup"), file("/dup")); err == nil {
		t.Error("duplicate path in batch accepted")
	}
	if err := c.Put(file("/p")); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(file("/p")); err == nil {
		t.Error("second insert of same path accepted")
	}
	if n := c.Counts()[Pending]; n != 1 {
		t.Fatalf("failed puts leaked into state: %d pending", n)
	}
}

func TestSnapshotCycle(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir, Options{SnapshotBytes: 512})
	for i := range 200 {
		f := file(fmt.Sprintf("/f/%03d", i))
		if err := c.Put(f); err != nil {
			t.Fatal(err)
		}
		if i%3 == 0 {
			c.Delete(f.ID)
		}
	}
	c.SetMeta("k", "v")
	st, _ := os.Stat(filepath.Join(dir, walFile))
	if st.Size() >= 512+200 {
		t.Fatalf("wal never reset: %d bytes", st.Size())
	}
	want := c.Counts()[Pending]
	c.Close()
	c = mustOpen(t, dir, Options{SnapshotBytes: 512})
	defer c.Close()
	if got := c.Counts()[Pending]; got != want || want != 133 {
		t.Fatalf("after reopen %d pending, want %d (133)", got, want)
	}
	if v, _ := c.Meta("k"); v != "v" {
		t.Fatal("meta lost across snapshot")
	}
}

func TestTornWALTail(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir, Options{})
	c.Put(file("/a"), file("/b"))
	c.Close()
	wal := filepath.Join(dir, walFile)
	b, _ := os.ReadFile(wal)
	good := len(b)
	// Half a frame, as if killed mid-write.
	frame := appendFrame(nil, encodePut(&File{ID: 9, Path: "/c", Kind: kind.Text, Status: Pending}))
	os.WriteFile(wal, append(b, frame[:len(frame)/2]...), 0o600)

	c = mustOpen(t, dir, Options{})
	if _, ok := c.Lookup("/a"); !ok {
		t.Fatal("committed record lost")
	}
	if _, ok := c.Lookup("/c"); ok {
		t.Fatal("torn record applied")
	}
	if st, _ := os.Stat(wal); st.Size() != int64(good) {
		t.Fatalf("wal not truncated: %d, want %d", st.Size(), good)
	}
	if err := c.Put(file("/c")); err != nil { // appends after the truncation point
		t.Fatal(err)
	}
	c.Close()
	c = mustOpen(t, dir, Options{})
	defer c.Close()
	if _, ok := c.Lookup("/c"); !ok {
		t.Fatal("record after recovery lost")
	}
}

func TestCorruptSnapshotFailsLoud(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir, Options{})
	c.Put(file("/a"))
	c.Snapshot()
	c.Close()
	p := filepath.Join(dir, snapFile)
	b, _ := os.ReadFile(p)
	b[len(b)-3] ^= 0xff
	os.WriteFile(p, b, 0o600)
	if _, err := Open(dir, Options{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

func TestCrashRecovery(t *testing.T) {
	if os.Getenv("CATALOG_CRASH_CHILD") != "" {
		crashChild()
		return
	}
	if testing.Short() {
		t.Skip("short")
	}
	dir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "committed")
	for round := range 8 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCrashRecovery$")
		cmd.Env = append(os.Environ(), "CATALOG_CRASH_CHILD=1", "CATALOG_DIR="+dir, "CATALOG_LOG="+logPath,
			"CATALOG_ROUND="+strconv.Itoa(round))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Duration(50+rand.IntN(150)) * time.Millisecond)
		cmd.Process.Signal(syscall.SIGKILL)
		cmd.Wait()

		c, err := Open(dir, Options{SnapshotBytes: 4096})
		if err != nil {
			t.Fatalf("round %d reopen: %v", round, err)
		}
		b, _ := os.ReadFile(logPath)
		text := string(b)
		text = text[:strings.LastIndexByte(text, '\n')+1]
		n := 0
		for line := range strings.Lines(text) {
			path := strings.TrimSpace(line)
			if _, ok := c.Lookup(path); !ok {
				t.Fatalf("round %d: committed %s missing", round, path)
			}
			n++
		}
		c.Close()
		if round == 7 && n == 0 {
			t.Fatal("child never committed anything")
		}
	}
}

func crashChild() {
	round := os.Getenv("CATALOG_ROUND")
	c, err := Open(os.Getenv("CATALOG_DIR"), Options{SnapshotBytes: 4096}) // snapshot often
	if err != nil {
		panic(err)
	}
	lf, err := os.OpenFile(os.Getenv("CATALOG_LOG"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		panic(err)
	}
	for i := 0; ; i++ {
		p := fmt.Sprintf("/r%s/%d", round, i)
		if err := c.Put(file(p)); err != nil {
			panic(err)
		}
		lf.WriteString(p + "\n")
		lf.Sync()
	}
}

func FuzzApply(f *testing.F) {
	full := &File{ID: 3, Path: "/x/y", Kind: kind.PDF, Status: Failed, Err: "boom", Gen: 9, NVec: 4, Size: 1, MTime: -5}
	f.Add(encodePut(full))
	f.Add(encodeDelete(3))
	f.Add(encodeMeta("a", "b"))
	f.Add([]byte{opPut, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, p []byte) {
		c := &Catalog{byID: map[uint64]*File{}, byPath: map[string]uint64{}, meta: map[string]string{}, nextID: 1}
		if err := c.apply(p); err != nil {
			return
		}
		for id, f := range c.byID {
			if err := f.validate(); err != nil || c.byPath[f.Path] != id {
				t.Fatalf("invalid state after apply: %+v (%v)", f, err)
			}
			// Round trip must be lossless.
			g, err := decodePut(encodePut(f)[1:])
			if err != nil || g != *f {
				t.Fatalf("round trip: %+v -> %+v (%v)", *f, g, err)
			}
		}
	})
}

func FuzzReadFrame(f *testing.F) {
	f.Add(appendFrame(nil, encodeMeta("k", "v")))
	f.Add([]byte{1, 0, 0, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		off := 0
		for off < len(b) {
			_, n, err := readFrame(b[off:])
			if err != nil {
				return
			}
			if n <= frameHeader || off+n > len(b) {
				t.Fatalf("bad frame length %d at %d of %d", n, off, len(b))
			}
			off += n
		}
	})
}

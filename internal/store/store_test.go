package store

import (
	"errors"
	"fmt"
	"math"
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

var testProf = Profile{Model: "test", Precision: "fp32", Dim: 32}

func randVec(r *rand.Rand, n int) []float32 {
	v := make([]float32, n)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	return v
}

func recs(r *rand.Rand, n, dim int) []Record {
	out := make([]Record, n)
	for i := range out {
		out[i] = Record{Vec: randVec(r, dim), Kind: kind.Text, Loc: kind.Locator{Kind: kind.LocBytes, Start: int64(i), End: int64(i + 1)}}
	}
	return out
}

func mustOpen(t testing.TB, dir string, live map[uint64]uint64, opt Options) *Store {
	t.Helper()
	s, err := Open(dir, testProf, live, opt)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s
}

func commit(t testing.TB, s *Store, fileID uint64, rs []Record) uint64 {
	t.Helper()
	gen, err := s.Append(fileID, rs)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.SetLive(fileID, gen); err != nil {
		t.Fatalf("setlive: %v", err)
	}
	return gen
}

func TestQuantize(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	v := randVec(r, 64)
	q := make([]int8, 32)
	sc, err := Quantize(q, v, 32)
	if err != nil {
		t.Fatal(err)
	}
	// Self-similarity of the quantized vector must be ~1.
	if got := sc * sc * float32(dot(q, q)); math.Abs(float64(got)-1) > 0.02 {
		t.Fatalf("self score %v, want ~1", got)
	}
	for _, bad := range [][]float32{make([]float32, 32), make([]float32, 8), append(make([]float32, 31), float32(math.NaN()))} {
		if _, err := Quantize(q, bad, 32); err == nil {
			t.Errorf("Quantize(%v) succeeded, want error", bad)
		}
	}
}

func TestSearchFindsExactVector(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	s := mustOpen(t, t.TempDir(), nil, Options{SegmentCap: 50})
	defer s.Close()
	var target []float32
	for f := uint64(1); f <= 20; f++ {
		rs := recs(r, 10, 64)
		if f == 13 {
			target = rs[7].Vec
		}
		commit(t, s, f, rs)
	}
	if st := s.Stats(); st.Segments < 4 || st.Records != 200 || st.Dead != 0 {
		t.Fatalf("stats %+v", st)
	}
	hits, err := s.Search(Query{Vec: target, K: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 5 || hits[0].FileID != 13 || hits[0].Loc.Start != 7 || hits[0].Score < 0.98 {
		t.Fatalf("top hit %+v", hits[0])
	}
	for i := 1; i < len(hits); i++ {
		if hits[i].Score > hits[i-1].Score {
			t.Fatalf("hits not sorted: %+v", hits)
		}
	}
	// Filters.
	hits, _ = s.Search(Query{Vec: target, K: 5, Accept: func(f uint64) bool { return f != 13 }})
	for _, h := range hits {
		if h.FileID == 13 {
			t.Fatalf("Accept filter ignored: %+v", h)
		}
	}
	hits, _ = s.Search(Query{Vec: target, K: 5, Kinds: kind.SetOf(kind.Image)})
	if len(hits) != 0 {
		t.Fatalf("kind filter ignored: %+v", hits)
	}
}

func TestReplaceDeleteAndUncommitted(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	dir := t.TempDir()
	s := mustOpen(t, dir, nil, Options{SegmentCap: 16})
	old := recs(r, 5, 32)
	commit(t, s, 1, old)
	newer := recs(r, 3, 32)
	g2 := commit(t, s, 1, newer)
	if n := s.Count(1); n != 3 {
		t.Fatalf("count after replace = %d, want 3", n)
	}
	hits, _ := s.Search(Query{Vec: old[0].Vec, K: 10})
	for _, h := range hits {
		if h.Gen != g2 {
			t.Fatalf("stale generation visible: %+v", h)
		}
	}
	// Appended but never made live: invisible.
	pending, err := s.Append(2, recs(r, 4, 32))
	if err != nil {
		t.Fatal(err)
	}
	if s.Count(2) != 0 {
		t.Fatal("uncommitted records counted")
	}
	if err := s.SetLive(1, 0); err != nil {
		t.Fatal(err)
	}
	if hits, _ := s.Search(Query{Vec: newer[0].Vec, K: 10}); len(hits) != 0 {
		t.Fatalf("deleted file still visible: %+v", hits)
	}
	if st := s.Stats(); st.Dead != st.Records {
		t.Fatalf("everything should be dead: %+v", st)
	}
	if err := s.SetLive(9, 1<<40); err == nil {
		t.Fatal("SetLive with unknown generation succeeded")
	}
	s.Close()

	// Reopen with the catalog view {2: pending}: those records become live.
	s = mustOpen(t, dir, map[uint64]uint64{2: pending}, Options{SegmentCap: 16})
	defer s.Close()
	if s.Count(2) != 4 || s.Count(1) != 0 {
		t.Fatalf("after reopen counts: f1=%d f2=%d", s.Count(1), s.Count(2))
	}
	// Fresh generations never collide with ones already on disk.
	if g, _ := s.Append(3, nil); g <= pending {
		t.Fatalf("generation %d reused (pending was %d)", g, pending)
	}
}

func TestProfileMismatch(t *testing.T) {
	dir := t.TempDir()
	mustOpen(t, dir, nil, Options{}).Close()
	p := testProf
	p.Precision = "q4"
	if _, err := Open(dir, p, nil, Options{}); !errors.Is(err, ErrProfileMismatch) {
		t.Fatalf("err = %v, want ErrProfileMismatch", err)
	}
}

func TestSingleOpener(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir, nil, Options{})
	defer s.Close()
	if _, err := Open(dir, testProf, nil, Options{}); err == nil {
		t.Fatal("second Open of the same dir succeeded")
	}
}

func TestCompaction(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	dir := t.TempDir()
	s := mustOpen(t, dir, nil, Options{SegmentCap: 10})
	live := map[uint64]uint64{}
	keep := map[uint64][]Record{}
	for f := uint64(1); f <= 10; f++ {
		rs := recs(r, 5, 32)
		live[f] = commit(t, s, f, rs)
		keep[f] = rs
	}
	for f := uint64(1); f <= 10; f += 2 { // delete half
		if err := s.SetLive(f, 0); err != nil {
			t.Fatal(err)
		}
		delete(live, f)
	}
	before := s.Stats()
	st, err := s.Compact(0.3)
	if err != nil {
		t.Fatal(err)
	}
	after := s.Stats()
	if st.Dropped == 0 || after.Records != before.Records-st.Dropped || after.Dead != before.Dead-st.Dropped {
		t.Fatalf("compaction %+v: before %+v after %+v", st, before, after)
	}
	check := func(s *Store) {
		t.Helper()
		for f, rs := range keep {
			want := 0
			if _, ok := live[f]; ok {
				want = 5
			}
			if n := s.Count(f); n != want {
				t.Fatalf("file %d count %d want %d", f, n, want)
			}
			if want > 0 {
				hits, _ := s.Search(Query{Vec: rs[2].Vec, K: 1})
				if len(hits) != 1 || hits[0].FileID != f || hits[0].Loc.Start != 2 {
					t.Fatalf("file %d not found after compaction: %+v", f, hits)
				}
			}
		}
	}
	check(s)
	// Appends still work and the store reopens identically.
	live[11] = commit(t, s, 11, recs(r, 3, 32))
	s.Close()
	s = mustOpen(t, dir, live, Options{SegmentCap: 10})
	defer s.Close()
	check(s)
	if s.Count(11) != 3 {
		t.Fatalf("file 11 count %d", s.Count(11))
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != s.Stats().Segments+2 { // + manifest + LOCK
		t.Fatalf("leftover files after compaction: %d entries for %d segments", len(ents), s.Stats().Segments)
	}
}

func TestTornWriteRecovery(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 10))
	dir := t.TempDir()
	s := mustOpen(t, dir, nil, Options{})
	g := commit(t, s, 1, recs(r, 4, 32))
	s.Close()
	// Simulate a crash mid-append: a full vector record, half a meta record.
	act := filepath.Join(dir, segDirName(1))
	appendJunk(t, filepath.Join(act, vecFile), make([]byte, vecRecSize(32)))
	appendJunk(t, filepath.Join(act, metaFile), make([]byte, metaRecSize/2))
	s = mustOpen(t, dir, map[uint64]uint64{1: g}, Options{})
	defer s.Close()
	if s.Count(1) != 4 {
		t.Fatalf("count %d after recovery", s.Count(1))
	}
	if _, err := s.Append(2, recs(r, 1, 32)); err != nil {
		t.Fatalf("append after recovery: %v", err)
	}
}

// A crash between creating the first segment and writing the manifest must
// not wedge the store.
func TestInterruptedInit(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, segDirName(1)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, segDirName(1), "vectors.bin"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := mustOpen(t, dir, nil, Options{})
	defer s.Close()
	commit(t, s, 1, recs(rand.New(rand.NewPCG(1, 2)), 3, testProf.Dim))
	if n := s.Count(1); n != 3 {
		t.Fatalf("count %d", n)
	}
}

func TestSealedCorruptionFailsLoud(t *testing.T) {
	r := rand.New(rand.NewPCG(11, 12))
	dir := t.TempDir()
	s := mustOpen(t, dir, nil, Options{SegmentCap: 4})
	commit(t, s, 1, recs(r, 6, 32))
	s.Close()
	p := filepath.Join(dir, segDirName(1), vecFile)
	b, _ := os.ReadFile(p)
	b[headerSize+5] ^= 0xff
	os.WriteFile(p, b, 0o600)
	if _, err := Open(dir, testProf, nil, Options{SegmentCap: 4}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

func appendJunk(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(b)
	f.Close()
}

// TestCrashRecovery SIGKILLs a writer process at random points and checks
// that every generation it reported as committed survives intact.
func TestCrashRecovery(t *testing.T) {
	if os.Getenv("STORE_CRASH_CHILD") != "" {
		crashChild()
		return
	}
	if testing.Short() {
		t.Skip("short")
	}
	dir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "committed")
	committed := map[uint64]uint64{}
	for round := range 8 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCrashRecovery$")
		cmd.Env = append(os.Environ(), "STORE_CRASH_CHILD=1", "STORE_DIR="+dir, "STORE_LOG="+logPath,
			"STORE_ROUND="+strconv.Itoa(round))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Duration(50+rand.IntN(150)) * time.Millisecond)
		cmd.Process.Signal(syscall.SIGKILL)
		cmd.Wait()

		committed = readCommitLog(t, logPath)
		s, err := Open(dir, testProf, committed, Options{SegmentCap: 64})
		if err != nil {
			t.Fatalf("round %d reopen: %v", round, err)
		}
		for f := range committed {
			if n := s.Count(f); n != 3 {
				t.Fatalf("round %d: file %d has %d live vectors, want 3", round, f, n)
			}
		}
		s.Close()
	}
	if len(committed) == 0 {
		t.Fatal("child never committed anything")
	}
}

// crashChild appends forever, logging (fileID, gen) after each Append returns.
// The log stands in for the catalog commit.
func crashChild() {
	dir, logPath := os.Getenv("STORE_DIR"), os.Getenv("STORE_LOG")
	round, _ := strconv.Atoi(os.Getenv("STORE_ROUND"))
	s, err := Open(dir, testProf, nil, Options{SegmentCap: 64})
	if err != nil {
		panic(err)
	}
	lf, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		panic(err)
	}
	r := rand.New(rand.NewPCG(uint64(round), 1))
	for i := uint64(1); ; i++ {
		f := uint64(round)*1_000_000 + i
		gen, err := s.Append(f, recs(r, 3, 32))
		if err != nil {
			panic(err)
		}
		lf.WriteString(strconv.FormatUint(f, 10) + " " + strconv.FormatUint(gen, 10) + "\n")
		lf.Sync()
	}
}

func readCommitLog(t *testing.T, path string) map[uint64]uint64 {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	m := map[uint64]uint64{}
	text := string(b)
	// A trailing partial line was never "committed".
	text = text[:strings.LastIndexByte(text, '\n')+1]
	for line := range strings.Lines(text) {
		var f, g uint64
		if _, err := fmt.Sscan(line, &f, &g); err != nil {
			t.Fatalf("commit log line %q: %v", line, err)
		}
		m[f] = g
	}
	return m
}

func FuzzLoadSegment(f *testing.F) {
	r := rand.New(rand.NewPCG(1, 1))
	var vb, mb []byte
	vb = append(vb, encodeHeader(vecMagic, 8)...)
	mb = append(mb, encodeHeader(metaMagic, 8)...)
	for i := range 3 {
		q := make([]int8, 8)
		sc, _ := Quantize(q, randVec(r, 8), 8)
		vb = appendVecRec(vb, sc, q)
		mb = appendMetaRec(mb, meta{fileID: uint64(i + 1), gen: 1, kind: kind.Text})
	}
	f.Add(vb, mb, true)
	f.Add(vb[:30], mb, false)
	f.Fuzz(func(t *testing.T, vb, mb []byte, active bool) {
		root := t.TempDir()
		dir := filepath.Join(root, segDirName(1))
		os.Mkdir(dir, 0o700)
		os.WriteFile(filepath.Join(dir, vecFile), vb, 0o600)
		os.WriteFile(filepath.Join(dir, metaFile), mb, 0o600)
		g, _, err := loadSegment(root, 1, 8, active)
		if err != nil {
			return
		}
		defer g.seal()
		if len(g.scales) != g.len() || len(g.vecs) != g.len()*8 {
			t.Fatalf("inconsistent segment: %d scales, %d vec bytes, %d metas", len(g.scales), len(g.vecs), g.len())
		}
		if active { // recovery must leave files consistent
			g.seal()
			g2, dropped, err := loadSegment(root, 1, 8, false)
			if err != nil || dropped != 0 || g2.len() != g.len() {
				t.Fatalf("after recovery: err=%v dropped=%d len=%d want %d", err, dropped, g2.len(), g.len())
			}
		}
	})
}

func BenchmarkSearch1M(b *testing.B) {
	const n, dim = 1_000_000, 256
	prof := Profile{Model: "bench", Precision: "fp32", Dim: dim}
	s, err := Open(b.TempDir(), prof, nil, Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	r := rand.New(rand.NewPCG(1, 2))
	const batch = 10_000
	for f := uint64(1); f <= n/batch; f++ {
		rs := make([]Record, batch)
		for i := range rs {
			rs[i] = Record{Vec: randVec(r, dim), Kind: kind.Text}
		}
		gen, err := s.Append(f, rs)
		if err != nil {
			b.Fatal(err)
		}
		s.SetLive(f, gen)
	}
	q := randVec(r, dim)
	b.ResetTimer()
	for b.Loop() {
		if _, err := s.Search(Query{Vec: q, K: 20}); err != nil {
			b.Fatal(err)
		}
	}
}

// Page locators survive a reopen (sealed segments and the active tail), and
// malformed locators are rejected before anything is written.
func TestPageLocator(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	dir := t.TempDir()
	s := mustOpen(t, dir, nil, Options{SegmentCap: 2})
	locs := []kind.Locator{
		{Kind: kind.LocPage, Page: 1, Start: 0, End: 40},
		{Kind: kind.LocPage, Page: kind.MaxPage}, // embedded as an image
		{Kind: kind.LocPage, Page: 7, Start: 100, End: 230},
	}
	rs := make([]Record, len(locs))
	for i, l := range locs {
		rs[i] = Record{Vec: randVec(r, 32), Kind: kind.PDF, Loc: l}
	}
	gen := commit(t, s, 9, rs)
	for _, bad := range []kind.Locator{
		{Kind: kind.LocPage},                            // no page
		{Kind: kind.LocPage, Page: 2, Start: 5, End: 5}, // empty non-zero range
		{Kind: kind.LocPage, Page: 2, Start: 6, End: 5},
		{Kind: kind.LocBytes, Page: 2, Start: 0, End: 5}, // page on a byte range
		{Kind: kind.LocNone, Page: 1},
	} {
		_, err := s.Append(10, []Record{{Vec: randVec(r, 32), Kind: kind.PDF, Loc: bad}})
		if !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("locator %+v: err %v, want ErrInvalidRecord", bad, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = mustOpen(t, dir, map[uint64]uint64{9: gen}, Options{SegmentCap: 2})
	defer s.Close()
	for i, rec := range rs {
		hits, err := s.Search(Query{Vec: rec.Vec, K: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].Loc != locs[i] || hits[0].Kind != kind.PDF {
			t.Fatalf("record %d: hit %+v, want %+v", i, hits, locs[i])
		}
	}
}

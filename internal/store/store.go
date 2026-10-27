// Package store is semd's embedded vector store: append-only int8 segments,
// generation-based liveness, brute-force parallel search and compaction.
//
// Liveness model. Every Append gets a fresh, store-wide generation number. A
// record is live only while its file's live generation (set via SetLive) equals
// the record's generation. The catalog is the source of truth for live
// generations and hands them to Open, so vectors from a write that never got
// committed to the catalog are dead after a crash, and nothing has to be
// persisted for deletes: superseded records are tombstoned implicitly.
//
// Commit protocol for replacing a file's vectors:
//
//	gen := s.Append(fileID, recs)   // durable, not yet visible
//	catalog.Put(..., gen)           // linearization point
//	s.SetLive(fileID, gen)          // old generation becomes dead
package store

import (
	"container/heap"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/msongd/kimquy/internal/fsutil"
	"github.com/msongd/kimquy/internal/kind"
)

const manifestFile = "manifest.json"

// ErrProfileMismatch means the store on disk was built with a different model,
// precision or dimension. The caller must reindex from scratch.
var ErrProfileMismatch = errors.New("store: embedding profile mismatch, reindex required")

// ErrInvalidRecord wraps validation failures of records passed to Append
// (bad kind, non-finite or zero vector). Nothing is written in that case.
var ErrInvalidRecord = errors.New("store: invalid record")

// Profile identifies the embedding space. Vectors from different profiles are
// not comparable, so a mismatch refuses to load.
type Profile struct {
	Model     string `json:"model"`
	Precision string `json:"precision"`
	Dim       int    `json:"dim"`
}

func (p Profile) validate() error {
	if p.Model == "" || p.Precision == "" {
		return errors.New("store: profile needs model and precision")
	}
	if p.Dim <= 0 || p.Dim > 4096 || p.Dim%8 != 0 {
		return fmt.Errorf("store: invalid dim %d (must be a positive multiple of 8, ≤4096)", p.Dim)
	}
	return nil
}

type manifest struct {
	Version     int      `json:"version"`
	Profile     Profile  `json:"profile"`
	Segments    []uint64 `json:"segments"` // last one is active
	NextSegment uint64   `json:"next_segment"`
}

// Options tune the store. Zero values pick defaults.
type Options struct {
	SegmentCap  int // records per segment before sealing (default 65536)
	Parallelism int // search goroutines (default GOMAXPROCS)
	Logger      *slog.Logger
}

// Record is one vector to append.
type Record struct {
	Vec  []float32 // full-length model output; truncated to Profile.Dim
	Kind kind.Kind
	Loc  kind.Locator
}

// Hit is one search result.
type Hit struct {
	FileID uint64
	Gen    uint64
	Kind   kind.Kind
	Loc    kind.Locator
	Score  float32
}

// Query describes a search.
type Query struct {
	Vec   []float32
	K     int
	Kinds kind.Set
	// Accept, if set, filters by file. It is called concurrently and must not block.
	Accept func(fileID uint64) bool
}

// fileGen counts one file's records of one generation in one segment.
type fileGen struct {
	seg *segment
	gen uint64
	n   int
}

type Store struct {
	dir  string
	opt  Options
	log  *slog.Logger
	lock *os.File

	compactMu sync.Mutex // serializes compactions

	mu     sync.RWMutex
	man    manifest
	segs   []*segment // ordered as man.Segments; last is active
	live   map[uint64]uint64
	index  map[uint64][]fileGen
	maxGen uint64
	broken error // set after a failed write; the store refuses further writes
	closed bool
}

// Open opens or creates a store in dir. live maps fileID to the generation the
// catalog considers current; every other record is treated as dead.
func Open(dir string, prof Profile, live map[uint64]uint64, opt Options) (*Store, error) {
	if err := prof.validate(); err != nil {
		return nil, err
	}
	if opt.SegmentCap <= 0 {
		opt.SegmentCap = 1 << 16
	}
	if opt.Parallelism <= 0 {
		opt.Parallelism = runtime.GOMAXPROCS(0)
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	lock, err := fsutil.LockDir(dir)
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, opt: opt, log: opt.Logger.With("component", "store"), lock: lock,
		live: make(map[uint64]uint64, len(live)), index: map[uint64][]fileGen{}}
	if err := s.load(prof); err != nil {
		lock.Close()
		s.closeSegments()
		return nil, err
	}
	for f, g := range live {
		if g != 0 {
			s.live[f] = g
		}
	}
	s.rebuildIndex()
	return s, nil
}

func (s *Store) load(prof Profile) error {
	b, err := os.ReadFile(filepath.Join(s.dir, manifestFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
		// No manifest means nothing was ever visible; segment dirs can only
		// be left by an initialization that crashed before saving it.
		if err := s.removeSegmentsExcept(nil); err != nil {
			return err
		}
		s.man = manifest{Version: 1, Profile: prof, NextSegment: 1}
		g, err := createSegment(s.dir, s.man.NextSegment, prof.Dim)
		if err != nil {
			return err
		}
		if err := g.openForAppend(); err != nil {
			return err
		}
		s.segs = []*segment{g}
		s.man.Segments = []uint64{g.id}
		s.man.NextSegment++
		return s.saveManifest()
	case err != nil:
		return fmt.Errorf("store: read manifest: %w", err)
	}
	if err := json.Unmarshal(b, &s.man); err != nil {
		return fmt.Errorf("%w: manifest: %v", ErrCorrupt, err)
	}
	if s.man.Version != 1 {
		return fmt.Errorf("%w: unsupported manifest version %d", ErrCorrupt, s.man.Version)
	}
	if s.man.Profile != prof {
		return fmt.Errorf("%w: on disk %+v, running %+v", ErrProfileMismatch, s.man.Profile, prof)
	}
	if len(s.man.Segments) == 0 {
		return fmt.Errorf("%w: manifest lists no segments", ErrCorrupt)
	}
	known := map[string]bool{}
	for i, id := range s.man.Segments {
		if id == 0 || id >= s.man.NextSegment {
			return fmt.Errorf("%w: manifest segment id %d out of range", ErrCorrupt, id)
		}
		if known[segDirName(id)] {
			return fmt.Errorf("%w: manifest lists segment %d twice", ErrCorrupt, id)
		}
		known[segDirName(id)] = true
		active := i == len(s.man.Segments)-1
		g, dropped, err := loadSegment(s.dir, id, prof.Dim, active)
		if err != nil {
			return err
		}
		if dropped > 0 {
			s.log.Warn("recovered torn write in active segment", "segment", id, "dropped_records", dropped)
		}
		s.segs = append(s.segs, g)
	}
	// Segments not in the manifest come from an interrupted compaction or
	// rotation; they were never visible, so delete them.
	return s.removeSegmentsExcept(known)
}

// removeSegmentsExcept deletes segment dirs whose names are not in keep.
func (s *Store) removeSegmentsExcept(keep map[string]bool) error {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	for _, e := range ents {
		if e.IsDir() && strings.HasPrefix(e.Name(), "seg-") && !keep[e.Name()] {
			s.log.Warn("removing orphan segment", "dir", e.Name())
			if err := os.RemoveAll(filepath.Join(s.dir, e.Name())); err != nil {
				return fmt.Errorf("store: remove orphan segment: %w", err)
			}
		}
	}
	return nil
}

func (s *Store) saveManifest() error {
	b, err := json.MarshalIndent(s.man, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(s.dir, manifestFile), b)
}

// rebuildIndex recomputes per-file generation counts, dead counters and maxGen
// from the in-memory segments. Caller holds mu (or has exclusive access).
func (s *Store) rebuildIndex() {
	clear(s.index)
	for _, g := range s.segs {
		g.dead = 0
		for _, m := range g.metas {
			s.maxGen = max(s.maxGen, m.gen)
			if s.live[m.fileID] != m.gen {
				g.dead++
			}
			fgs := s.index[m.fileID]
			if n := len(fgs); n > 0 && fgs[n-1].seg == g && fgs[n-1].gen == m.gen {
				fgs[n-1].n++
			} else {
				s.index[m.fileID] = append(fgs, fileGen{seg: g, gen: m.gen, n: 1})
			}
		}
	}
	// Drop index entries for generations that can never become live again.
	for f, fgs := range s.index {
		lg := s.live[f]
		fgs = slices.DeleteFunc(fgs, func(e fileGen) bool { return e.gen != lg })
		if len(fgs) == 0 {
			delete(s.index, f)
		} else {
			s.index[f] = fgs
		}
	}
}

func (s *Store) writable() error {
	if s.closed {
		return errors.New("store: closed")
	}
	if s.broken != nil {
		return fmt.Errorf("store: disabled after write failure: %w", s.broken)
	}
	return nil
}

// Append durably writes recs for fileID under a fresh generation and returns
// it. The records stay invisible until SetLive(fileID, gen). An empty recs is
// allowed and still yields a generation (for files with no content).
func (s *Store) Append(fileID uint64, recs []Record) (uint64, error) {
	if fileID == 0 {
		return 0, errors.New("store: fileID 0 is reserved")
	}
	dim := s.man.Profile.Dim // immutable after Open
	scales := make([]float32, len(recs))
	vecs := make([]int8, len(recs)*dim)
	metas := make([]meta, len(recs))
	for i, r := range recs {
		if !r.Kind.Valid() {
			return 0, fmt.Errorf("%w %d: kind %v", ErrInvalidRecord, i, r.Kind)
		}
		if err := r.Loc.Validate(); err != nil {
			return 0, fmt.Errorf("%w %d: %v", ErrInvalidRecord, i, err)
		}
		sc, err := Quantize(vecs[i*dim:(i+1)*dim], r.Vec, dim)
		if err != nil {
			return 0, fmt.Errorf("%w %d: %v", ErrInvalidRecord, i, err)
		}
		scales[i] = sc
		metas[i] = meta{fileID: fileID, kind: r.Kind, loc: r.Loc}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writable(); err != nil {
		return 0, err
	}
	s.maxGen++
	gen := s.maxGen
	for i := range metas {
		metas[i].gen = gen
	}
	for off := 0; off < len(metas); {
		act := s.segs[len(s.segs)-1]
		if act.len() >= s.opt.SegmentCap {
			if err := s.rotate(); err != nil {
				s.broken = err
				return 0, err
			}
			continue
		}
		n := min(len(metas)-off, s.opt.SegmentCap-act.len())
		if err := act.write(scales[off:off+n], vecs[off*dim:(off+n)*dim], metas[off:off+n]); err != nil {
			s.broken = err
			return 0, err
		}
		act.dead += n // not live until SetLive
		s.index[fileID] = append(s.index[fileID], fileGen{seg: act, gen: gen, n: n})
		off += n
	}
	return gen, nil
}

// rotate seals the active segment and starts a new one. Caller holds mu.
func (s *Store) rotate() error {
	act := s.segs[len(s.segs)-1]
	if err := act.seal(); err != nil {
		return fmt.Errorf("store: seal segment %d: %w", act.id, err)
	}
	id := s.man.NextSegment
	g, err := createSegment(s.dir, id, s.man.Profile.Dim)
	if err != nil {
		return err
	}
	if err := g.openForAppend(); err != nil {
		return err
	}
	s.man.NextSegment++
	s.man.Segments = append(s.man.Segments, id)
	if err := s.saveManifest(); err != nil {
		return err
	}
	s.segs = append(s.segs, g)
	return nil
}

// SetLive makes gen the visible generation of fileID; gen 0 deletes the file.
// gen must be 0 or a generation returned by Append for this file.
func (s *Store) SetLive(fileID, gen uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("store: closed")
	}
	old := s.live[fileID]
	if old == gen {
		return nil
	}
	fgs := s.index[fileID]
	if gen != 0 && !slices.ContainsFunc(fgs, func(e fileGen) bool { return e.gen == gen }) &&
		gen > s.maxGen {
		return fmt.Errorf("store: SetLive(%d, %d): unknown generation", fileID, gen)
	}
	for _, e := range fgs {
		switch e.gen {
		case old:
			e.seg.dead += e.n
		case gen:
			e.seg.dead -= e.n
		}
	}
	keep := max(old, gen)
	fgs = slices.DeleteFunc(fgs, func(e fileGen) bool { return e.gen != gen && e.gen <= keep })
	if len(fgs) == 0 {
		delete(s.index, fileID)
	} else {
		s.index[fileID] = fgs
	}
	if gen == 0 {
		delete(s.live, fileID)
	} else {
		s.live[fileID] = gen
	}
	return nil
}

// Count returns how many live vectors fileID has.
func (s *Store) Count(fileID uint64) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	lg, ok := s.live[fileID]
	if !ok {
		return 0
	}
	n := 0
	for _, e := range s.index[fileID] {
		if e.gen == lg {
			n += e.n
		}
	}
	return n
}

// Stats summarizes the store.
type Stats struct {
	Segments int `json:"segments"`
	Records  int `json:"records"`
	Dead     int `json:"dead"`
	Files    int `json:"files"`
}

func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Stats{Segments: len(s.segs), Files: len(s.live)}
	for _, g := range s.segs {
		st.Records += g.len()
		st.Dead += g.dead
	}
	return st
}

// Search returns the K best live records by approximate cosine similarity.
func (s *Store) Search(q Query) ([]Hit, error) {
	if q.K <= 0 || q.K > 10000 {
		return nil, fmt.Errorf("store: k=%d out of range", q.K)
	}
	dim := s.man.Profile.Dim
	qv := make([]int8, dim)
	qs, err := Quantize(qv, q.Vec, dim)
	if err != nil {
		return nil, fmt.Errorf("store: query: %w", err)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errors.New("store: closed")
	}
	type span struct {
		g      *segment
		lo, hi int
	}
	// Split work into roughly equal spans so small and large segments balance.
	total := 0
	for _, g := range s.segs {
		total += g.len()
	}
	step := max(4096, (total+s.opt.Parallelism-1)/s.opt.Parallelism)
	var spans []span
	for _, g := range s.segs {
		for lo := 0; lo < g.len(); lo += step {
			spans = append(spans, span{g, lo, min(g.len(), lo+step)})
		}
	}

	work := make(chan span)
	results := make(chan hitHeap, s.opt.Parallelism)
	var wg sync.WaitGroup
	for range min(s.opt.Parallelism, max(1, len(spans))) {
		wg.Go(func() {
			h := make(hitHeap, 0, q.K)
			for sp := range work {
				h = s.scan(h, sp.g, sp.lo, sp.hi, qv, qs, &q)
			}
			results <- h
		})
	}
	for _, sp := range spans {
		work <- sp
	}
	close(work)
	wg.Wait()
	close(results)

	all := make(hitHeap, 0, q.K)
	for h := range results {
		for _, x := range h {
			all = pushTopK(all, x, q.K)
		}
	}
	slices.SortFunc(all, func(a, b Hit) int {
		switch {
		case a.Score > b.Score:
			return -1
		case a.Score < b.Score:
			return 1
		}
		return 0
	})
	return all, nil
}

// scan scores records [lo, hi) of g into h. Liveness and filters are checked
// only for records that would enter the heap, which keeps the hot loop tight.
func (s *Store) scan(h hitHeap, g *segment, lo, hi int, qv []int8, qs float32, q *Query) hitHeap {
	dim := g.dim
	for i := lo; i < hi; i++ {
		score := qs * g.scales[i] * float32(dot(qv, g.vecs[i*dim:(i+1)*dim]))
		if len(h) == q.K && score <= h[0].Score {
			continue
		}
		m := &g.metas[i]
		if s.live[m.fileID] != m.gen || !q.Kinds.Has(m.kind) || (q.Accept != nil && !q.Accept(m.fileID)) {
			continue
		}
		h = pushTopK(h, Hit{FileID: m.fileID, Gen: m.gen, Kind: m.kind, Loc: m.loc, Score: score}, q.K)
	}
	return h
}

// CompactStats reports what a compaction did.
type CompactStats struct {
	Compacted []uint64 // segment ids rewritten
	Written   []uint64 // new segment ids
	Dropped   int      // dead records reclaimed
}

// Compact rewrites sealed segments whose dead fraction exceeds threshold
// (e.g. 0.3). Searches and appends continue while new segments are written;
// only the final swap takes the write lock.
func (s *Store) Compact(threshold float64) (CompactStats, error) {
	var st CompactStats
	if threshold < 0 || threshold >= 1 {
		return st, fmt.Errorf("store: compaction threshold %v out of [0,1)", threshold)
	}
	s.compactMu.Lock()
	defer s.compactMu.Unlock()

	s.mu.Lock()
	if err := s.writable(); err != nil {
		s.mu.Unlock()
		return st, err
	}
	var victims []*segment
	for _, g := range s.segs {
		if g.sealed && g.len() > 0 && float64(g.dead)/float64(g.len()) > threshold {
			victims = append(victims, g)
		}
	}
	if len(victims) == 0 {
		s.mu.Unlock()
		return st, nil
	}
	dim := s.man.Profile.Dim
	var scales []float32
	var vecs []int8
	var metas []meta
	for _, g := range victims {
		for i, m := range g.metas {
			if s.live[m.fileID] == m.gen {
				scales = append(scales, g.scales[i])
				vecs = append(vecs, g.vecs[i*dim:(i+1)*dim]...)
				metas = append(metas, m)
			} else {
				st.Dropped++
			}
		}
		st.Compacted = append(st.Compacted, g.id)
	}
	nseg := (len(metas) + s.opt.SegmentCap - 1) / s.opt.SegmentCap
	firstID := s.man.NextSegment
	s.man.NextSegment += uint64(nseg)
	s.mu.Unlock()

	// Sealed victims are immutable, so writing their copies needs no lock.
	var fresh []*segment
	for i := range nseg {
		lo, hi := i*s.opt.SegmentCap, min(len(metas), (i+1)*s.opt.SegmentCap)
		g, err := writeSealedSegment(s.dir, firstID+uint64(i), dim, scales[lo:hi], vecs[lo*dim:hi*dim], metas[lo:hi])
		if err != nil {
			return st, fmt.Errorf("store: compaction: %w", err)
		}
		fresh = append(fresh, g)
		st.Written = append(st.Written, g.id)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return st, errors.New("store: closed during compaction")
	}
	isVictim := map[uint64]bool{}
	for _, g := range victims {
		isVictim[g.id] = true
	}
	var segs []*segment
	var ids []uint64
	for i, g := range s.segs {
		if isVictim[g.id] {
			continue
		}
		if i == len(s.segs)-1 { // keep active last
			segs = append(segs, fresh...)
			ids = append(ids, st.Written...)
		}
		segs = append(segs, g)
		ids = append(ids, g.id)
	}
	prev := s.man.Segments
	s.man.Segments = ids
	if err := s.saveManifest(); err != nil {
		s.man.Segments = prev
		s.broken = err
		return st, err
	}
	s.segs = segs
	s.rebuildIndex()
	for _, g := range victims {
		if err := os.RemoveAll(g.dir); err != nil {
			s.log.Error("compaction: remove old segment", "dir", g.dir, "err", err)
		}
	}
	return st, nil
}

func (s *Store) closeSegments() {
	for _, g := range s.segs {
		if !g.sealed {
			for _, f := range []*os.File{g.vf, g.mf} {
				if f != nil {
					f.Close()
				}
			}
		}
	}
}

// Close flushes and releases the store.
func (s *Store) Close() error {
	s.compactMu.Lock()
	defer s.compactMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var errs []error
	for _, g := range s.segs {
		if !g.sealed {
			for _, f := range []*os.File{g.vf, g.mf} {
				if f != nil {
					errs = append(errs, f.Sync(), f.Close())
				}
			}
			g.vf, g.mf = nil, nil
		}
	}
	errs = append(errs, s.lock.Close())
	return errors.Join(errs...)
}

// hitHeap is a min-heap on Score used for top-k selection.
type hitHeap []Hit

func (h hitHeap) Len() int           { return len(h) }
func (h hitHeap) Less(i, j int) bool { return h[i].Score < h[j].Score }
func (h hitHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *hitHeap) Push(x any)        { *h = append(*h, x.(Hit)) }
func (h *hitHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

func pushTopK(h hitHeap, x Hit, k int) hitHeap {
	if len(h) < k {
		heap.Push(&h, x)
	} else if x.Score > h[0].Score {
		h[0] = x
		heap.Fix(&h, 0)
	}
	return h
}

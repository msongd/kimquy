// Package engine is semd's core: it owns the config, catalog, vector store
// and work queue, runs the crawl → extract → chunk → embed → commit pipeline,
// and answers searches. The API layer is a thin shell around it.
//
// Error policy: process returns nil when a file is done (indexed, removed, or
// its failure recorded in the catalog), a transient error when the path should
// be retried, and any other error only for broken invariants (catalog or
// store writes failing). Those are fatal: Run returns them and semd exits.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/msongd/kimquy/internal/catalog"
	"github.com/msongd/kimquy/internal/config"
	"github.com/msongd/kimquy/internal/embed"
	"github.com/msongd/kimquy/internal/events"
	"github.com/msongd/kimquy/internal/kind"
	"github.com/msongd/kimquy/internal/sched"
	"github.com/msongd/kimquy/internal/store"
	"github.com/msongd/kimquy/internal/watch"
)

const (
	// StoreDim is the Matryoshka truncation used for stored vectors.
	StoreDim = 256

	pauseManual   = "manual"
	pauseReindex  = "reindex in progress"
	pauseNoStore  = "reindex required"
	compactEvery  = 10 * time.Minute
	compactThresh = 0.3
)

// badRootEvery is how often roots that could not be read are tried again.
var badRootEvery = time.Minute // var for tests

// ErrReindexRequired is returned by Search while the store cannot be used.
var ErrReindexRequired = errors.New("index unusable, reindex required")

// errTransient marks errors after which the path is retried later.
var errTransient = errors.New("transient")

func transient(err error) error { return fmt.Errorf("%w: %w", errTransient, err) }

// Options configure an Engine.
type Options struct {
	StateDir string // holds config.json, catalog/, store/
	Embedder embed.Embedder
	QueueCap int // default 1024
	Logger   *slog.Logger
	Bus      *events.Bus
	// Watch, if set, reports file changes so they are indexed promptly and
	// a restart replays only what changed. Without it, every start and
	// root change does a full scan.
	Watch watch.System
	// Power, if set, is polled every PowerEvery (default 10 s); indexing
	// pauses on battery (if configured), in Low Power Mode, or when hot.
	Power      func() (sched.Power, error)
	PowerEvery time.Duration
}

type Engine struct {
	dir string
	emb embed.Embedder
	// media is the embedder's media side, if it has one.
	media embed.MediaEmbedder
	info  embed.Info
	prof  store.Profile
	log   *slog.Logger
	bus   *events.Bus
	cat   *catalog.Catalog
	q     *sched.Queue

	cfgSave sync.Mutex // serializes config changes, so none is lost
	cfgMu   sync.RWMutex
	cfg     config.Config

	storeMu  sync.RWMutex // RLock while using store; Lock to replace it
	store    *store.Store // nil while a reindex is required
	storeErr error

	probMu   sync.Mutex
	problems map[string]string // path → error for things not in the catalog
	badRoots map[string]string

	jobsMu sync.Mutex
	jobs   map[string]*job // path → file being processed

	scanning  atomic.Int32 // full and subtree scans running
	scanReq   chan struct{}
	scanReqN  atomic.Uint64 // full scans requested …
	scanDoneN atomic.Uint64 // … and the request count the last finished one covered

	watch        watch.System // nil: no change notifications
	watchStatus  atomic.Value // string
	rootsChanged chan struct{}

	power      func() (sched.Power, error)
	powerEvery time.Duration
}

// Open loads state from opt.StateDir. A store built with a different
// embedding profile does not prevent startup: the engine comes up with
// indexing paused and Status reporting that a reindex is required.
func Open(opt Options) (*Engine, error) {
	if opt.Embedder == nil {
		return nil, errors.New("engine: no embedder")
	}
	if opt.QueueCap <= 0 {
		opt.QueueCap = 1024
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.PowerEvery <= 0 {
		opt.PowerEvery = 10 * time.Second
	}
	if opt.Bus == nil {
		opt.Bus = events.NewBus()
	}
	info := opt.Embedder.Info()
	if info.Dims < StoreDim || info.MaxBatch < 1 || info.Model == "" || info.Precision == "" || info.Kinds == 0 {
		return nil, fmt.Errorf("engine: unusable embedder info %+v", info)
	}
	media, _ := opt.Embedder.(embed.MediaEmbedder)
	if media == nil && (info.Kinds.Has(kind.Image) || info.Kinds.Has(kind.PDF) || info.Kinds.Has(kind.Audio) || info.Kinds.Has(kind.Video)) {
		return nil, errors.New("engine: embedder claims media support but cannot read media")
	}
	if err := os.MkdirAll(opt.StateDir, 0o700); err != nil {
		return nil, err
	}
	e := &Engine{
		dir: opt.StateDir, emb: opt.Embedder, media: media, info: info, log: opt.Logger, bus: opt.Bus,
		prof:     store.Profile{Model: info.Model, Precision: info.Precision, Dim: StoreDim},
		q:        sched.NewQueue(opt.QueueCap),
		problems: map[string]string{}, badRoots: map[string]string{}, jobs: map[string]*job{},
		scanReq: make(chan struct{}, 1),
		watch:   opt.Watch, rootsChanged: make(chan struct{}, 1),
		power: opt.Power, powerEvery: opt.PowerEvery,
	}
	e.watchStatus.Store(watchOff)
	e.q.OnPause(func(reasons []string) {
		msg := "resumed"
		if len(reasons) > 0 {
			msg = strings.Join(reasons, ", ")
		}
		e.bus.Publish(events.Event{Type: "pause", Msg: msg})
	})
	cfg, err := config.Load(e.configPath())
	if err != nil {
		return nil, err
	}
	e.cfg = cfg
	if e.cat, err = catalog.Open(filepath.Join(e.dir, "catalog"), catalog.Options{Logger: e.log}); err != nil {
		return nil, err
	}
	st, err := store.Open(e.storeDir(), e.prof, e.cat.LiveGens(), store.Options{Logger: e.log})
	switch {
	case errors.Is(err, store.ErrProfileMismatch) || errors.Is(err, store.ErrCorrupt):
		e.log.Error("vector store unusable; indexing paused until reindex", "err", err)
		e.storeErr = err
		e.q.Pause(pauseNoStore)
	case err != nil:
		e.cat.Close()
		return nil, err
	default:
		e.store = st
		if err := e.reconcile(); err != nil {
			e.Close()
			return nil, err
		}
	}
	return e, nil
}

func (e *Engine) configPath() string { return filepath.Join(e.dir, "config.json") }
func (e *Engine) storeDir() string   { return filepath.Join(e.dir, "store") }

// reconcile marks files whose live vector count disagrees with the catalog
// for re-indexing, e.g. after a damaged store segment was truncated.
func (e *Engine) reconcile() error {
	var fix []*catalog.File
	e.cat.Each(func(f catalog.File) bool {
		if f.Status == catalog.Indexed && e.store.Count(f.ID) != int(f.NVec) {
			e.log.Warn("vector count mismatch, will reindex", "path", f.Path, "catalog", f.NVec, "store", e.store.Count(f.ID))
			f.Status = catalog.Pending
			fix = append(fix, &f)
		}
		return true
	})
	return putBatched(e.cat, fix)
}

func putBatched(cat *catalog.Catalog, fs []*catalog.File) error {
	for len(fs) > 0 {
		n := min(len(fs), 1000)
		if err := cat.Put(fs[:n]...); err != nil {
			return err
		}
		fs = fs[n:]
	}
	return nil
}

// Config returns a copy of the current configuration.
func (e *Engine) Config() config.Config {
	e.cfgMu.RLock()
	defer e.cfgMu.RUnlock()
	c := e.cfg
	c.Roots = append([]string(nil), c.Roots...)
	c.Excludes = append([]string(nil), c.Excludes...)
	return c
}

// SetRoots replaces roots and excludes, persists them, and triggers a scan.
// Roots must exist and are stored by their real (symlink-free) path.
func (e *Engine) SetRoots(roots, excludes []string) (config.Config, error) {
	e.cfgSave.Lock()
	defer e.cfgSave.Unlock()
	c := e.Config()
	c.Roots = make([]string, 0, len(roots))
	for _, r := range roots {
		if !filepath.IsAbs(r) {
			return c, fmt.Errorf("root %q is not absolute", r)
		}
		real, err := filepath.EvalSymlinks(r)
		if err != nil {
			return c, fmt.Errorf("root %q: %w", r, err)
		}
		if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
			return c, fmt.Errorf("root %q is not a directory", r)
		}
		c.Roots = append(c.Roots, real)
	}
	if excludes != nil {
		c.Excludes = excludes
	}
	if err := config.Save(e.configPath(), c); err != nil {
		return c, err
	}
	e.cfgMu.Lock()
	e.cfg = c
	e.cfgMu.Unlock()
	if e.watch != nil {
		select { // the watch loop restarts its stream, then scans
		case e.rootsChanged <- struct{}{}:
		default:
		}
	} else {
		e.RequestScan()
	}
	return e.Config(), nil
}

// SetPauseOnBattery changes and persists whether indexing pauses on
// battery; the power monitor picks it up at its next poll.
func (e *Engine) SetPauseOnBattery(on bool) (config.Config, error) {
	e.cfgSave.Lock()
	defer e.cfgSave.Unlock()
	c := e.Config()
	c.PauseOnBattery = on
	if err := config.Save(e.configPath(), c); err != nil {
		return e.Config(), err
	}
	e.cfgMu.Lock()
	e.cfg = c
	e.cfgMu.Unlock()
	return e.Config(), nil
}

// RequestScan asks the scanner to walk all roots; requests coalesce.
func (e *Engine) RequestScan() {
	e.scanReqN.Add(1)
	select {
	case e.scanReq <- struct{}{}:
	default:
	}
}

// Pause and Resume control indexing manually.
func (e *Engine) Pause()  { e.q.Pause(pauseManual) }
func (e *Engine) Resume() { e.q.Resume(pauseManual) }

// Run processes work until ctx ends or a fatal error occurs.
func (e *Engine) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	fatal := func(err error) {
		e.log.Error("fatal", "err", err)
		e.bus.Publish(events.Event{Type: "fatal", Msg: err.Error()})
		cancel(err)
	}
	var wg sync.WaitGroup
	for range e.Config().Workers {
		wg.Go(func() { e.worker(ctx, fatal) })
	}
	wg.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-e.scanReq:
			}
			covers := e.scanReqN.Load()
			e.scanning.Add(1)
			e.bus.Publish(events.Event{Type: "scan", Msg: "started"})
			err := e.scan(ctx, "")
			e.scanning.Add(-1)
			if err != nil {
				if ctx.Err() == nil {
					fatal(fmt.Errorf("scan: %w", err))
				}
				return
			}
			e.scanDoneN.Store(covers)
			e.bus.Publish(events.Event{Type: "scan", Msg: "finished"})
		}
	})
	// A root may become readable without a change event: Full Disk Access
	// granted, a volume mounted. Look again now and then.
	wg.Go(func() {
		t := time.NewTicker(badRootEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			cfg := e.Config()
			e.probMu.Lock()
			bad := slices.Sorted(maps.Keys(e.badRoots))
			e.probMu.Unlock()
			for _, r := range bad {
				if !slices.Contains(cfg.Roots, r) {
					continue
				}
				if err := e.subScan(ctx, r); err != nil {
					if ctx.Err() == nil {
						fatal(err)
					}
					return
				}
			}
		}
	})
	wg.Go(func() {
		t := time.NewTicker(compactEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := e.compact(); err != nil {
					fatal(err)
					return
				}
			}
		}
	})
	if e.power != nil {
		wg.Go(func() {
			m := sched.PowerMonitor{
				Probe:          e.power,
				PauseOnBattery: func() bool { return e.Config().PauseOnBattery },
				Every:          e.powerEvery,
				Log:            e.log,
			}
			if err := m.Run(ctx, e.q); err != nil && ctx.Err() == nil {
				fatal(err)
			}
		})
	}
	if e.watch != nil {
		wg.Go(func() {
			if err := e.runWatch(ctx); err != nil && ctx.Err() == nil {
				fatal(err)
			}
		})
	} else {
		e.RequestScan()
	}
	<-ctx.Done()
	e.q.Close()
	wg.Wait()
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func (e *Engine) worker(ctx context.Context, fatal func(error)) {
	backoff := time.Second
	for {
		p, err := e.q.Next(ctx)
		if err != nil {
			return
		}
		jctx, j := e.startJob(ctx, p)
		err = e.process(jctx, p)
		e.endJob(j)
		retry := err != nil && ctx.Err() == nil && errors.Is(err, errTransient)
		if retry {
			// p is in flight, so Add only marks it and never blocks; Done
			// then requeues it. The queue is never empty in between, which
			// the watch checkpoint relies on.
			e.q.Add(ctx, p)
		}
		e.q.Done(p)
		switch {
		case err == nil:
			backoff = time.Second
		case ctx.Err() != nil:
			return
		case errors.Is(err, errTransient):
			e.log.Warn("will retry", "path", p, "err", err, "in", backoff)
			e.bus.Publish(events.Event{Type: "retry", Path: p, Msg: err.Error()})
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, time.Minute)
		default:
			fatal(fmt.Errorf("processing %s: %w", p, err))
			return
		}
	}
}

func (e *Engine) compact() error {
	e.storeMu.RLock()
	defer e.storeMu.RUnlock()
	if e.store == nil {
		return nil
	}
	st, err := e.store.Compact(compactThresh)
	if err != nil {
		return fmt.Errorf("compaction: %w", err)
	}
	if len(st.Compacted) > 0 {
		e.log.Info("compacted", "segments", len(st.Compacted), "reclaimed", st.Dropped)
	}
	return nil
}

// Reindex discards all vectors and re-embeds every file. It is also the way
// out of a profile mismatch (model or precision change).
func (e *Engine) Reindex() error {
	e.q.Pause(pauseReindex)
	defer e.q.Resume(pauseReindex)
	e.q.Clear()

	e.storeMu.Lock() // waits for in-flight files
	defer e.storeMu.Unlock()
	if e.store != nil {
		if err := e.store.Close(); err != nil {
			e.log.Error("closing old store", "err", err)
		}
		e.store = nil
	}
	if err := os.RemoveAll(e.storeDir()); err != nil {
		e.storeErr = err
		return fmt.Errorf("reindex: remove store: %w", err)
	}
	var reset []*catalog.File
	e.cat.Each(func(f catalog.File) bool {
		f.Status, f.Gen, f.NVec, f.Err = catalog.Pending, 0, 0, ""
		reset = append(reset, &f)
		return true
	})
	if err := putBatched(e.cat, reset); err != nil {
		e.storeErr = err
		return fmt.Errorf("reindex: reset catalog: %w", err)
	}
	st, err := store.Open(e.storeDir(), e.prof, nil, store.Options{Logger: e.log})
	if err != nil {
		e.storeErr = err
		return fmt.Errorf("reindex: %w", err)
	}
	e.store, e.storeErr = st, nil
	e.q.Resume(pauseNoStore)
	e.probMu.Lock()
	clear(e.problems)
	e.probMu.Unlock()
	e.bus.Publish(events.Event{Type: "reindex", Msg: "started"})
	e.RequestScan()
	return nil
}

// Close releases the catalog and store. Call after Run returns.
func (e *Engine) Close() error {
	e.storeMu.Lock()
	defer e.storeMu.Unlock()
	var errs []error
	if e.store != nil {
		errs = append(errs, e.store.Close())
		e.store = nil
	}
	return errors.Join(append(errs, e.cat.Close())...)
}

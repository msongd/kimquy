package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/msongd/kimquy/internal/catalog"
	"github.com/msongd/kimquy/internal/config"
	"github.com/msongd/kimquy/internal/events"
	"github.com/msongd/kimquy/internal/fsutil"
	"github.com/msongd/kimquy/internal/kind"
	"github.com/msongd/kimquy/internal/watch"
)

// Watch states reported by Status.
const (
	watchOff       = "off"       // no change notifications: scans only
	watchIdle      = "idle"      // no roots to watch
	watchReplaying = "replaying" // catching up on changes made while down
	watchLive      = "live"
)

// checkpointEvery is how often the replay point is saved once all work
// up to it is done.
var checkpointEvery = 2 * time.Second // var for tests

// watchState is persisted in watch.json. EventID is a point up to which
// every change under Roots has been indexed; it is only valid while each
// root's volume keeps the same event history.
type watchState struct {
	EventID uint64            `json:"event_id"`
	Roots   []string          `json:"roots"`
	History map[string]string `json:"history"` // root → FSEvents history UUID
}

func (e *Engine) watchPath() string { return filepath.Join(e.dir, "watch.json") }

func (e *Engine) loadWatchState() (watchState, bool) {
	var s watchState
	data, err := os.ReadFile(e.watchPath())
	if errors.Is(err, os.ErrNotExist) {
		return s, false
	}
	if err == nil {
		err = json.Unmarshal(data, &s)
	}
	if err == nil && (s.EventID == 0 || len(s.History) != len(s.Roots)) {
		err = errors.New("incomplete")
	}
	if err != nil {
		e.log.Error("ignoring unreadable watch state; rescanning", "path", e.watchPath(), "err", err)
		return watchState{}, false
	}
	return s, true
}

// histories identifies each root's event history; ok is false if any root's
// volume keeps none (or the root is missing), in which case changes cannot
// be replayed.
func (e *Engine) histories(roots []string) (map[string]string, bool) {
	h := make(map[string]string, len(roots))
	for _, r := range roots {
		id, err := e.watch.History(r)
		if err != nil {
			e.log.Warn("cannot replay changes for root; it is rescanned at startup", "root", r, "err", err)
			return nil, false
		}
		h[r] = id
	}
	return h, true
}

// canResume reports whether saved covers exactly roots with unchanged histories.
func canResume(saved watchState, roots []string, hist map[string]string) bool {
	return hist != nil && slices.Equal(saved.Roots, roots) && maps.Equal(saved.History, hist)
}

// watcher is the state of the watch loop, owned by its goroutine.
type watcher struct {
	e      *Engine
	stream watch.Stream
	roots  []string
	saved  watchState // last persisted (zero if none)
	seen   uint64     // highest event ID handled
	// fresh is set when the stream (re)starts: the saved state no longer
	// describes it even if the event ID is unchanged.
	fresh bool
	// hist identifies the roots' event histories; nil if some root has
	// none, in which case nothing is saved and every start rescans.
	hist map[string]string
}

// runWatch keeps a stream over the configured roots and turns events into
// queued paths and scans. It returns only on ctx end or a fatal error.
func (e *Engine) runWatch(ctx context.Context) error {
	w := &watcher{e: e}
	w.saved, _ = e.loadWatchState()
	defer func() {
		w.checkpoint()
		w.close()
	}()
	if err := w.start(ctx, true); err != nil {
		return err
	}
	tick := time.NewTicker(checkpointEvery)
	defer tick.Stop()
	for {
		var ready <-chan struct{}
		if w.stream != nil {
			ready = w.stream.Ready()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-e.rootsChanged:
			if err := w.start(ctx, false); err != nil {
				return err
			}
		case <-ready:
			evs, overflow := w.stream.Take()
			if err := w.handle(ctx, evs, overflow); err != nil {
				return err
			}
		case <-tick.C:
			w.checkpoint()
		}
	}
}

func (w *watcher) close() {
	if w.stream != nil {
		if err := w.stream.Close(); err != nil {
			w.e.log.Warn("closing event stream", "err", err)
		}
		w.stream = nil
	}
}

// start (re)opens the stream over the configured roots. At startup it
// resumes from the saved event ID when it can; otherwise it watches from
// now on and requests a full scan, which covers everything before.
func (w *watcher) start(ctx context.Context, startup bool) error {
	w.close()
	e := w.e
	w.roots = slices.Clone(e.Config().Roots)
	if len(w.roots) == 0 {
		e.watchStatus.Store(watchIdle)
		w.seen = 0
		e.RequestScan() // drops everything indexed under former roots
		return nil
	}
	w.hist, _ = e.histories(w.roots)
	since := w.saved.EventID
	resume := startup && canResume(w.saved, w.roots, w.hist)
	if !resume {
		since = e.watch.Now() // before the scan, so nothing falls in between
		e.RequestScan()
	}
	s, err := e.watch.Watch(w.roots, since)
	if err != nil {
		return fmt.Errorf("watch %v: %w", w.roots, err)
	}
	w.stream, w.seen, w.fresh = s, since, true
	if resume {
		e.watchStatus.Store(watchReplaying)
		e.log.Info("replaying changes since last run", "event_id", since)
		e.bus.Publish(events.Event{Type: "watch", Msg: "replaying"})
		// The previous run's full scan may have queued files it never got to.
		if err := e.queueUnfinished(ctx); err != nil {
			return ctx.Err()
		}
	} else {
		e.watchStatus.Store(watchLive)
		e.bus.Publish(events.Event{Type: "watch", Msg: "live"})
	}
	return nil
}

// queueUnfinished queues catalog entries that are neither indexed nor failed.
func (e *Engine) queueUnfinished(ctx context.Context) error {
	var err error
	e.cat.Each(func(f catalog.File) bool {
		if f.Status != catalog.Indexed && f.Status != catalog.Failed {
			err = e.q.Add(ctx, f.Path)
		}
		return err == nil
	})
	return err
}

// handle turns one batch of events into work. Only after all of it is
// queued (or scanned) does seen advance past the batch.
func (w *watcher) handle(ctx context.Context, evs []watch.Event, overflow bool) error {
	e := w.e
	full := overflow
	if overflow {
		e.log.Warn("event buffer overflowed; rescanning")
	}
	var dirs, files []string
	maxID := w.seen
	for _, ev := range evs {
		maxID = max(maxID, ev.ID)
		f := ev.Flags
		switch {
		case f&watch.HistoryDone != 0:
			if e.watchStatus.Load() == watchReplaying {
				e.watchStatus.Store(watchLive)
				e.log.Info("replay complete")
				e.bus.Publish(events.Event{Type: "watch", Msg: "live"})
			}
		case f.Lost() || f&(watch.RootChanged|watch.Mount|watch.Unmount) != 0:
			e.log.Warn("change notifications incomplete; rescanning", "path", ev.Path, "flags", f)
			full = true
		case f&watch.MustScanSubDirs != 0:
			dirs = append(dirs, ev.Path)
		case f&watch.IsDir != 0:
			if f&(watch.Created|watch.Removed|watch.Renamed) != 0 {
				dirs = append(dirs, ev.Path)
			}
		default: // files and symlinks
			files = append(files, ev.Path)
		}
	}
	if full {
		e.RequestScan()
	} else {
		for _, d := range outermost(dirs) {
			if err := e.subScan(ctx, d); err != nil {
				return err
			}
		}
		cfg := e.Config()
		for _, p := range files {
			if !e.wants(&cfg, p) {
				continue
			}
			if err := e.q.Add(ctx, p); err != nil {
				return ctx.Err()
			}
		}
	}
	w.seen = maxID
	return nil
}

// outermost drops paths lying below another path of the list.
func outermost(dirs []string) []string {
	slices.Sort(dirs)
	dirs = slices.Compact(dirs)
	var out []string
	for _, d := range dirs {
		if len(out) == 0 || !config.Within(out[len(out)-1], d) {
			out = append(out, d)
		}
	}
	return out
}

// wants reports whether a changed file may need indexing or removal.
func (e *Engine) wants(cfg *config.Config, p string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	if _, ok := e.cat.Lookup(p); ok {
		return true // process re-checks and removes if needed
	}
	root, ok := cfg.RootOf(p)
	if !ok || cfg.ExcludedPath(root, p) {
		return false
	}
	k := kind.FromPath(p)
	return (k.Valid() && e.info.Kinds.Has(k)) || (k == kind.Unknown && filepath.Ext(p) == "")
}

// subScan scans the directory tree at dir, in the calling goroutine.
func (e *Engine) subScan(ctx context.Context, dir string) error {
	e.scanning.Add(1)
	defer e.scanning.Add(-1)
	if err := e.scan(ctx, dir); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("scan %s: %w", dir, err)
	}
	return nil
}

// checkpoint persists seen once everything up to it is indexed: no scan
// pending or running and the queue drained. Otherwise a crash could lose
// changes that were taken from the stream but never processed.
func (w *watcher) checkpoint() {
	e := w.e
	id := w.seen
	if w.stream == nil || id == 0 || w.hist == nil {
		return
	}
	// Replaying changes would never index a root that could not be read,
	// e.g. before Full Disk Access was granted: let the next start rescan.
	e.probMu.Lock()
	bad := len(e.badRoots) > 0
	e.probMu.Unlock()
	if bad {
		if w.saved.EventID != 0 {
			if err := os.Remove(e.watchPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
				e.log.Error("removing watch state", "err", err)
				return
			}
			w.saved = watchState{}
		}
		return
	}
	if e.q.Len() != 0 || e.scanReqN.Load() != e.scanDoneN.Load() || e.scanning.Load() != 0 {
		return
	}
	if id == w.saved.EventID && !w.fresh {
		return
	}
	s := watchState{EventID: id, Roots: slices.Clone(w.roots), History: w.hist}
	data, err := json.Marshal(s)
	if err != nil {
		panic(err) // plain data
	}
	if err := fsutil.WriteFileAtomic(e.watchPath(), data); err != nil {
		e.log.Error("saving watch state", "err", err)
		return
	}
	w.saved, w.fresh = s, false
	e.log.Debug("watch checkpoint", "event_id", id)
}

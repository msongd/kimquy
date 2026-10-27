package engine

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"time"

	"github.com/msongd/kimquy/internal/catalog"
	"github.com/msongd/kimquy/internal/chunk"
	"github.com/msongd/kimquy/internal/config"
	"github.com/msongd/kimquy/internal/crawl"
	"github.com/msongd/kimquy/internal/embed"
	"github.com/msongd/kimquy/internal/events"
	"github.com/msongd/kimquy/internal/extract"
	"github.com/msongd/kimquy/internal/kind"
	"github.com/msongd/kimquy/internal/store"
)

var errTooBig = errors.New("over size cap")

// scan walks all roots (or, when under is not "", only that subtree),
// queues new and changed files, and queues catalog entries that were not
// seen so a worker can confirm and remove them.
func (e *Engine) scan(ctx context.Context, under string) error {
	cfg := e.Config()
	seen := map[uint64]bool{}
	problems := map[string]string{}
	badRoots := map[string]string{}
	err := crawl.Walk(ctx, &cfg, e.info.Kinds, under, crawl.Visitor{
		File: func(_, p string, info fs.FileInfo) error {
			if old, ok := e.cat.Lookup(p); ok {
				seen[old.ID] = true
				if (old.Status == catalog.Indexed || old.Status == catalog.Failed) &&
					old.Size == info.Size() && old.MTime == info.ModTime().UnixNano() {
					return nil
				}
			}
			return e.q.Add(ctx, p)
		},
		Problem: func(root, p string, err error) {
			if p == root {
				// Unreadable roots are retried every badRootEvery: log
				// only news.
				e.probMu.Lock()
				known := e.badRoots[root] == err.Error()
				e.probMu.Unlock()
				if !known {
					e.log.Warn("cannot read root", "root", root, "err", err)
				}
				badRoots[root] = err.Error()
			} else {
				e.log.Warn("scan problem", "path", p, "err", err)
				problems[p] = err.Error()
			}
		},
	})
	if err != nil {
		return err
	}
	in := func(p string) bool { return under == "" || config.Within(under, p) }
	var addErr error
	e.cat.Each(func(f catalog.File) bool {
		if seen[f.ID] || !in(f.Path) {
			return true
		}
		// Keep entries of roots we could not read (e.g. an unmounted volume).
		if root, ok := cfg.RootOf(f.Path); ok && badRoots[root] != "" {
			return true
		}
		addErr = e.q.Add(ctx, f.Path)
		return addErr == nil
	})
	e.probMu.Lock()
	for p := range e.problems {
		if in(p) {
			delete(e.problems, p)
		}
	}
	for r := range e.badRoots {
		if in(r) {
			delete(e.badRoots, r)
		}
	}
	maps.Copy(e.problems, problems)
	maps.Copy(e.badRoots, badRoots)
	e.probMu.Unlock()
	return addErr
}

func (e *Engine) process(ctx context.Context, path string) error {
	e.storeMu.RLock()
	defer e.storeMu.RUnlock()
	st := e.store
	if st == nil {
		return transient(ErrReindexRequired)
	}
	cfg := e.Config()
	old, have := e.cat.Lookup(path)
	root, ok := cfg.RootOf(path)
	if !ok || cfg.ExcludedPath(root, path) {
		return e.remove(st, old, have, "outside roots or excluded")
	}
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return e.remove(st, old, have, "deleted")
	}
	k := kind.FromPath(path)
	if err != nil {
		return e.fail(st, path, k, nil, old, have, err)
	}
	if !fi.Mode().IsRegular() {
		return e.remove(st, old, have, "not a regular file")
	}
	if k == kind.Unknown && filepath.Ext(path) == "" {
		k = kind.Text // sniffed below
	}
	if !k.Valid() || !e.info.Kinds.Has(k) {
		return e.remove(st, old, have, "kind not indexable")
	}
	size, mtime := fi.Size(), fi.ModTime().UnixNano()
	if have && old.Status == catalog.Indexed && old.Size == size && old.MTime == mtime {
		return nil
	}

	var (
		sum  [sha256.Size]byte
		recs []store.Record
		note string
	)
	switch k {
	case kind.Image, kind.PDF, kind.Audio, kind.Video:
		// The sidecar reads the file itself; hash it without holding it in memory.
		sum, err = hashStable(path, cfg.Limit(k), fi)
	default:
		var data []byte
		if data, err = readStable(path, cfg.Limit(k), fi); err == nil {
			sum = sha256.Sum256(data)
			if have && old.Status == catalog.Indexed && old.Hash == sum {
				break
			}
			var text string
			text, err = e.extractText(path, k, data)
			if errors.Is(err, extract.ErrNotText) && filepath.Ext(path) == "" {
				return e.remove(st, old, have, "binary file without extension")
			}
			if err == nil {
				recs, err = e.embedText(ctx, filepath.Base(path), k, text)
				err = embedErr(ctx, err)
			}
		}
	}
	if err != nil {
		if errors.Is(err, errTransient) || errors.Is(err, embed.ErrFatal) {
			return err
		}
		return e.fail(st, path, k, fi, old, have, err)
	}
	if have && old.Status == catalog.Indexed && old.Hash == sum {
		old.Size, old.MTime, old.Updated = size, mtime, time.Now().UnixNano()
		return e.cat.Put(&old) // touched, content unchanged
	}
	if k == kind.Image || k == kind.PDF || k == kind.Audio || k == kind.Video {
		if recs, note, err = e.embedMedia(ctx, path, k); err != nil {
			if err = embedErr(ctx, err); errors.Is(err, errTransient) || errors.Is(err, embed.ErrFatal) {
				return err
			}
			return e.fail(st, path, k, fi, old, have, err)
		}
		// The sidecar read the file after we hashed it; make sure it is the same file.
		if err := unchanged(path, fi); err != nil {
			return err
		}
	}

	id := old.ID
	if !have {
		id = e.cat.NewID()
	}
	gen, err := st.Append(id, recs)
	if errors.Is(err, store.ErrInvalidRecord) {
		return e.fail(st, path, k, fi, old, have, fmt.Errorf("embedder output rejected: %w", err))
	}
	if err != nil {
		return err
	}
	f := catalog.File{ID: id, Path: path, Kind: k, Size: size, MTime: mtime, Hash: sum,
		Status: catalog.Indexed, Gen: gen, NVec: uint32(len(recs)), Updated: time.Now().UnixNano()}
	if err := e.cat.Put(&f); err != nil {
		return err
	}
	if err := st.SetLive(id, gen); err != nil {
		return err
	}
	e.clearProblem(path)
	msg := fmt.Sprintf("%d vectors", len(recs))
	if note != "" {
		e.log.Warn("indexed partially", "path", path, "why", note)
		msg += "; " + note
	}
	e.bus.Publish(events.Event{Type: "indexed", Path: path, Msg: msg})
	return nil
}

// readStable reads at most limit bytes (0 = unlimited) and returns a
// transient error if the file changed while it was read.
func readStable(path string, limit int64, before fs.FileInfo) ([]byte, error) {
	if limit > 0 && before.Size() > limit {
		return nil, fmt.Errorf("%w: %d bytes, cap is %d", errTooBig, before.Size(), limit)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := io.Reader(f)
	if limit > 0 {
		r = io.LimitReader(f, limit+1)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if limit > 0 && int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: grew past %d bytes", errTooBig, limit)
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if after.Size() != int64(len(data)) || !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		return nil, transient(errors.New("file changed while reading"))
	}
	return data, nil
}

// hashStable hashes at most limit bytes (0 = unlimited) of path, streaming,
// and returns a transient error if the file changed meanwhile.
func hashStable(path string, limit int64, before fs.FileInfo) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	if limit > 0 && before.Size() > limit {
		return sum, fmt.Errorf("%w: %d bytes, cap is %d", errTooBig, before.Size(), limit)
	}
	f, err := os.Open(path)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	h := sha256.New()
	r := io.Reader(f)
	if limit > 0 {
		r = io.LimitReader(f, limit+1)
	}
	n, err := io.Copy(h, r)
	if err != nil {
		return sum, err
	}
	if limit > 0 && n > limit {
		return sum, fmt.Errorf("%w: grew past %d bytes", errTooBig, limit)
	}
	if n != before.Size() {
		return sum, transient(errors.New("file changed while reading"))
	}
	if err := unchanged(path, before); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// unchanged returns a transient error if path no longer matches before.
func unchanged(path string, before fs.FileInfo) error {
	after, err := os.Stat(path)
	if err != nil {
		return transient(fmt.Errorf("file vanished while indexing: %w", err))
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return transient(errors.New("file changed while indexing"))
	}
	return nil
}

// embedErr classifies an embedder error: unavailability and cancellation
// are retried, fatal errors stop the daemon, anything else fails the file.
func embedErr(ctx context.Context, err error) error {
	if err == nil || errors.Is(err, errTransient) || errors.Is(err, embed.ErrFatal) {
		return err
	}
	if errors.Is(err, embed.ErrUnavailable) || ctx.Err() != nil {
		return transient(err)
	}
	return err
}

func (e *Engine) extractText(path string, k kind.Kind, data []byte) (string, error) {
	switch k {
	case kind.Text:
		return extract.Text(data)
	case kind.Office:
		return extract.Office(data, filepath.Ext(path))
	}
	return "", fmt.Errorf("no extractor for %s", k)
}

func (e *Engine) embedText(ctx context.Context, title string, k kind.Kind, text string) ([]store.Record, error) {
	var b chunkBatcher
	for _, c := range chunk.Split(text, chunk.Options{}) {
		b.add(c.Text, kind.Locator{Kind: kind.LocBytes, Start: int64(c.Start), End: int64(c.End)})
	}
	return b.flush(ctx, e, title, k, nil)
}

// chunkBatcher collects text chunks and embeds them in model-sized batches.
type chunkBatcher struct {
	texts []string
	locs  []kind.Locator
}

func (b *chunkBatcher) add(text string, loc kind.Locator) {
	b.texts = append(b.texts, text)
	b.locs = append(b.locs, loc)
}

// flushFull embeds the full batches collected so far, keeping the rest,
// and appends the records to recs.
func (b *chunkBatcher) flushFull(ctx context.Context, e *Engine, title string, k kind.Kind, recs []store.Record) ([]store.Record, error) {
	n := len(b.texts) / e.info.MaxBatch * e.info.MaxBatch
	recs, err := e.embedChunks(ctx, title, k, b.texts[:n], b.locs[:n], recs)
	b.texts, b.locs = b.texts[n:], b.locs[n:]
	return recs, err
}

// flush embeds everything collected and appends the records to recs.
func (b *chunkBatcher) flush(ctx context.Context, e *Engine, title string, k kind.Kind, recs []store.Record) ([]store.Record, error) {
	recs, err := e.embedChunks(ctx, title, k, b.texts, b.locs, recs)
	b.texts, b.locs = nil, nil
	return recs, err
}

func (e *Engine) embedChunks(ctx context.Context, title string, k kind.Kind, texts []string, locs []kind.Locator, recs []store.Record) ([]store.Record, error) {
	for i := 0; i < len(texts); i += e.info.MaxBatch {
		end := min(len(texts), i+e.info.MaxBatch)
		prompts := make([]string, 0, end-i)
		for _, t := range texts[i:end] {
			prompts = append(prompts, embed.DocPrompt(title, t))
		}
		vecs, err := e.emb.EmbedTexts(ctx, embed.Index, prompts)
		if err != nil {
			return nil, err
		}
		if len(vecs) != len(prompts) {
			return nil, fmt.Errorf("embedder returned %d vectors for %d inputs", len(vecs), len(prompts))
		}
		for j, v := range vecs {
			if len(v) != e.info.Dims {
				return nil, fmt.Errorf("embedder returned %d dims, announced %d", len(v), e.info.Dims)
			}
			recs = append(recs, store.Record{Vec: v, Kind: k, Loc: locs[i+j]})
		}
	}
	return recs, nil
}

// maxRenderedPages caps how many text-less pages of one PDF are embedded as
// images; beyond it they are skipped and reported.
const maxRenderedPages = 64

// embedMedia embeds an image, PDF, audio or video file through the media embedder. note
// describes content that was skipped under a documented cap.
func (e *Engine) embedMedia(ctx context.Context, path string, k kind.Kind) (recs []store.Record, note string, err error) {
	if e.media == nil {
		return nil, "", fmt.Errorf("embedder cannot read %s files", k)
	}
	switch k {
	case kind.Image:
		v, err := e.media.EmbedImage(ctx, embed.Index, path)
		if err != nil {
			return nil, "", err
		}
		if len(v) != e.info.Dims {
			return nil, "", fmt.Errorf("embedder returned %d dims, announced %d", len(v), e.info.Dims)
		}
		return []store.Record{{Vec: v, Kind: k}}, "", nil
	case kind.PDF:
		return e.embedPDF(ctx, path)
	case kind.Audio, kind.Video:
		return e.embedAV(ctx, path, k)
	}
	return nil, "", fmt.Errorf("no media embedder for %s", k)
}

func (e *Engine) embedPDF(ctx context.Context, path string) ([]store.Record, string, error) {
	title := filepath.Base(path)
	var (
		recs       []store.Record
		b          chunkBatcher
		budget     = maxRenderedPages
		pageCount  = -1
		unrendered int
	)
	for from := 1; pageCount < 0 || from <= pageCount; from += embed.MaxPDFPagesPerCall {
		res, err := e.media.ExtractPDF(ctx, embed.Index, embed.PDFRequest{
			Path: path, From: from, Count: embed.MaxPDFPagesPerCall, Render: min(budget, embed.MaxPDFPagesPerCall)})
		if err != nil {
			return nil, "", err
		}
		if pageCount >= 0 && res.PageCount != pageCount {
			return nil, "", transient(fmt.Errorf("page count changed from %d to %d while indexing", pageCount, res.PageCount))
		}
		pageCount = res.PageCount
		for _, pg := range res.Pages {
			switch {
			case pg.Vec != nil:
				budget--
				recs = append(recs, store.Record{Vec: pg.Vec, Kind: kind.PDF, Loc: kind.Locator{Kind: kind.LocPage, Page: uint16(pg.Page)}})
			case pg.Unrendered:
				unrendered++
			default:
				for _, c := range chunk.Split(pg.Text, chunk.Options{}) {
					b.add(c.Text, kind.Locator{Kind: kind.LocPage, Page: uint16(pg.Page), Start: int64(c.Start), End: int64(c.End)})
				}
			}
		}
		if recs, err = b.flushFull(ctx, e, title, kind.PDF, recs); err != nil {
			return nil, "", err
		}
	}
	recs, err := b.flush(ctx, e, title, kind.PDF, recs)
	if err != nil {
		return nil, "", err
	}
	note := ""
	if unrendered > 0 {
		note = fmt.Sprintf("%d text-less pages not embedded (cap %d per PDF)", unrendered, maxRenderedPages)
	}
	return recs, note, nil
}

// remove drops a file from the index if it was there.
func (e *Engine) remove(st *store.Store, old catalog.File, have bool, why string) error {
	if !have {
		return nil
	}
	if err := e.cat.Delete(old.ID); err != nil {
		return err
	}
	if err := st.SetLive(old.ID, 0); err != nil {
		return err
	}
	e.clearProblem(old.Path)
	e.bus.Publish(events.Event{Type: "removed", Path: old.Path, Msg: why})
	return nil
}

// fail records a permanent per-file failure; the file's vectors are dropped so
// stale content never shows up. Files of unknown kind go to the problem list.
func (e *Engine) fail(st *store.Store, path string, k kind.Kind, fi fs.FileInfo, old catalog.File, have bool, cause error) error {
	e.log.Warn("index failed", "path", path, "err", cause)
	e.bus.Publish(events.Event{Type: "failed", Path: path, Msg: cause.Error()})
	if !k.Valid() {
		e.setProblem(path, cause)
		return e.remove(st, old, have, "unreadable")
	}
	id := old.ID
	if !have {
		id = e.cat.NewID()
	}
	msg := cause.Error()
	if len(msg) > 1024 {
		msg = msg[:1024]
	}
	f := catalog.File{ID: id, Path: path, Kind: k, Status: catalog.Failed, Err: msg, Updated: time.Now().UnixNano()}
	if fi != nil {
		f.Size, f.MTime = fi.Size(), fi.ModTime().UnixNano()
	}
	if err := e.cat.Put(&f); err != nil {
		return err
	}
	return st.SetLive(id, 0)
}

func (e *Engine) setProblem(path string, err error) {
	e.probMu.Lock()
	e.problems[path] = err.Error()
	e.probMu.Unlock()
}

func (e *Engine) clearProblem(path string) {
	e.probMu.Lock()
	delete(e.problems, path)
	e.probMu.Unlock()
}

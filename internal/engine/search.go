package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/msongd/kimquy/internal/catalog"
	"github.com/msongd/kimquy/internal/config"
	"github.com/msongd/kimquy/internal/embed"
	"github.com/msongd/kimquy/internal/events"
	"github.com/msongd/kimquy/internal/kind"
	"github.com/msongd/kimquy/internal/store"
)

const (
	MaxK          = 100
	maxQueryBytes = 2000
	snippetBytes  = 300
)

// SearchRequest is a validated search.
type SearchRequest struct {
	Q     string
	K     int
	Roots []string // restrict to files under these directories
	Kinds kind.Set
}

// Hit is one result: the best-matching fragment of one file.
type Hit struct {
	Path    string       `json:"path"`
	Kind    string       `json:"kind"`
	Locator kind.Locator `json:"locator"`
	Score   float32      `json:"score"`
	Snippet string       `json:"snippet,omitempty"`
	Stale   bool         `json:"stale,omitempty"` // file changed since it was indexed
}

// Validate checks bounds and normalizes the request.
func (r *SearchRequest) Validate() error {
	r.Q = strings.TrimSpace(r.Q)
	switch {
	case r.Q == "":
		return errors.New("empty query")
	case len(r.Q) > maxQueryBytes:
		return fmt.Errorf("query longer than %d bytes", maxQueryBytes)
	case !utf8.ValidString(r.Q):
		return errors.New("query is not valid UTF-8")
	case r.K < 1 || r.K > MaxK:
		return fmt.Errorf("k=%d out of [1,%d]", r.K, MaxK)
	}
	for i, root := range r.Roots {
		if !filepath.IsAbs(root) {
			return fmt.Errorf("root filter %q is not absolute", root)
		}
		r.Roots[i] = filepath.Clean(root)
	}
	return nil
}

// Search embeds the query and returns the best fragment per file.
func (e *Engine) Search(ctx context.Context, req SearchRequest) ([]Hit, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	vecs, err := e.emb.EmbedTexts(ctx, embed.Query, []string{embed.QueryPrompt(req.Q)})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("embed query: got %d vectors", len(vecs))
	}

	e.storeMu.RLock()
	defer e.storeMu.RUnlock()
	if e.store == nil {
		return nil, fmt.Errorf("%w: %v", ErrReindexRequired, e.storeErr)
	}
	q := store.Query{Vec: vecs[0], K: min(4*req.K, 10*MaxK), Kinds: req.Kinds}
	if len(req.Roots) > 0 {
		q.Accept = func(id uint64) bool {
			f, ok := e.cat.Get(id)
			return ok && slices.ContainsFunc(req.Roots, func(r string) bool { return config.Within(r, f.Path) })
		}
	}
	raw, err := e.store.Search(q)
	if err != nil {
		return nil, err
	}
	texts := map[string]string{} // extracted text per path (and page), for snippets
	seen := map[uint64]bool{}
	var hits []Hit
	for _, h := range raw {
		if seen[h.FileID] {
			continue
		}
		f, ok := e.cat.Get(h.FileID)
		if !ok || f.Gen != h.Gen {
			continue // replaced or removed since the store was read
		}
		seen[h.FileID] = true
		hit := Hit{Path: f.Path, Kind: h.Kind.String(), Locator: h.Loc, Score: h.Score}
		hit.Snippet, hit.Stale = e.snippet(ctx, f, h.Loc, texts)
		hits = append(hits, hit)
		if len(hits) == req.K {
			break
		}
	}
	return hits, nil
}

// snippet reads the fragment live from disk. stale is true when the file no
// longer matches what was indexed, in which case the snippet may be off.
func (e *Engine) snippet(ctx context.Context, f catalog.File, loc kind.Locator, cache map[string]string) (snip string, stale bool) {
	fi, err := os.Stat(f.Path)
	if err != nil {
		return "", true
	}
	stale = fi.Size() != f.Size || fi.ModTime().UnixNano() != f.MTime
	switch {
	case loc.Kind == kind.LocPage && loc.End > 0:
		return e.pageSnippet(ctx, f, loc, cache), stale
	case loc.Kind != kind.LocBytes:
		return "", stale
	}
	text, ok := cache[f.Path]
	if !ok {
		cfg := e.Config()
		data, err := readStable(f.Path, cfg.Limit(f.Kind), fi)
		if err == nil {
			text, err = e.extractText(f.Path, f.Kind, data)
		}
		if err != nil {
			return "", true
		}
		cache[f.Path] = text
	}
	s, end := int(loc.Start), int(loc.End)
	if s < 0 || end > len(text) || s >= end {
		return "", true
	}
	return compact(text[s:end], snippetBytes), stale
}

// pageSnippet asks the embedder for the page's text again. Extraction is
// deterministic, so the byte range still applies unless the file changed.
// Failures only cost the snippet: the hit itself is still valid.
func (e *Engine) pageSnippet(ctx context.Context, f catalog.File, loc kind.Locator, cache map[string]string) string {
	if e.media == nil {
		return ""
	}
	key := fmt.Sprintf("%s\x00%d", f.Path, loc.Page)
	text, ok := cache[key]
	if !ok {
		res, err := e.media.ExtractPDF(ctx, embed.Query, embed.PDFRequest{Path: f.Path, From: int(loc.Page), Count: 1})
		if err != nil || len(res.Pages) != 1 {
			e.log.Debug("pdf snippet", "path", f.Path, "page", loc.Page, "err", err)
			return ""
		}
		text = res.Pages[0].Text
		cache[key] = text
	}
	s, end := int(loc.Start), int(loc.End)
	if s < 0 || end > len(text) || s >= end {
		return ""
	}
	return compact(text[s:end], snippetBytes)
}

// compact collapses whitespace and cuts s to at most n bytes on a rune boundary.
func compact(s string, n int) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if r == utf8.RuneError {
			continue
		}
		if unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		if b.Len()+utf8.RuneLen(r) > n {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Status is a snapshot of the engine's state.
type Status struct {
	Indexed    int          `json:"indexed"`
	Pending    int          `json:"pending"` // queued + in flight + marked for reindex
	Failed     int          `json:"failed"`
	Scanning   bool         `json:"scanning"`
	Watch      string       `json:"watch"` // off | idle | replaying | live
	Paused     []string     `json:"paused,omitempty"`
	Embedder   EmbedStatus  `json:"embedder"`
	Store      *store.Stats `json:"store,omitempty"`
	StoreError string       `json:"store_error,omitempty"`
	Roots      []RootStatus `json:"roots"`
}

type EmbedStatus struct {
	Model     string `json:"model"`
	Precision string `json:"precision"`
	Dims      int    `json:"dims"`
}

type RootStatus struct {
	Path  string `json:"path"`
	Error string `json:"error,omitempty"`
}

func (e *Engine) Status() Status {
	counts := e.cat.Counts()
	s := Status{
		Indexed:  counts[catalog.Indexed],
		Pending:  counts[catalog.Pending] + e.q.Len(),
		Failed:   counts[catalog.Failed],
		Scanning: e.scanning.Load() != 0,
		Watch:    e.watchStatus.Load().(string),
		Paused:   e.q.PausedReasons(),
		Embedder: EmbedStatus{Model: e.info.Model, Precision: e.info.Precision, Dims: e.info.Dims},
	}
	e.storeMu.RLock()
	if e.store != nil {
		st := e.store.Stats()
		s.Store = &st
	} else if e.storeErr != nil {
		s.StoreError = e.storeErr.Error()
	}
	e.storeMu.RUnlock()
	e.probMu.Lock()
	for _, r := range e.Config().Roots {
		s.Roots = append(s.Roots, RootStatus{Path: r, Error: e.badRoots[r]})
	}
	e.probMu.Unlock()
	return s
}

// Failure is a file or directory that could not be indexed.
type Failure struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// Failures lists catalog failures and scan problems, sorted by path.
func (e *Engine) Failures() []Failure {
	var out []Failure
	e.cat.Each(func(f catalog.File) bool {
		if f.Status == catalog.Failed {
			out = append(out, Failure{f.Path, f.Err})
		}
		return true
	})
	e.probMu.Lock()
	for _, p := range slices.Sorted(maps.Keys(e.problems)) {
		out = append(out, Failure{p, e.problems[p]})
	}
	for r, err := range e.badRoots {
		out = append(out, Failure{r, err})
	}
	e.probMu.Unlock()
	slices.SortFunc(out, func(a, b Failure) int { return cmp.Compare(a.Path, b.Path) })
	return out
}

// Events exposes the event bus for streaming.
func (e *Engine) Events() *events.Bus { return e.bus }

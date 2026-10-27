package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/msongd/kimquy/internal/embed"
	"github.com/msongd/kimquy/internal/fixture"
	"github.com/msongd/kimquy/internal/kind"
)

// The retrieval eval: index testdata/eval/corpus.json with the real model
// through the whole pipeline, run its queries and compare the scores with
// testdata/eval/baseline.json. Runs only with the model available:
//
//	SEMD_EMBED_BIN=embedder/.build-xcode/Build/Products/Release/semd-embed \
//	SEMD_MODEL=models/release go test ./internal/engine -run Eval -v
//
// SEMD_EVAL_UPDATE=1 rewrites the baseline instead of comparing.

const (
	evalK = 10
	// evalSlack is how far a metric may fall below the baseline: GPU
	// arithmetic is not bit-exact, so near ties can flip.
	evalSlack = 0.02
)

type evalDoc struct {
	Path    string          `json:"path"`
	Text    string          `json:"text,omitempty"`
	Pages   []fixture.Page  `json:"pages,omitempty"`
	Shapes  []fixture.Shape `json:"shapes,omitempty"`
	Checker bool            `json:"checker,omitempty"`
}

type evalQuery struct {
	Q    string `json:"q"`
	Want string `json:"want"`           // "" when nothing in the corpus answers it
	Page int    `json:"page,omitempty"` // PDF targets: the page that answers
	Lang string `json:"lang,omitempty"` // default en
}

type evalCorpus struct {
	Docs    []evalDoc   `json:"docs"`
	Queries []evalQuery `json:"queries"`
}

// evalScores are retrieval metrics over a set of queries.
type evalScores struct {
	N       int     `json:"n"`
	Top1    float64 `json:"top1"`
	Recall5 float64 `json:"recall5"`
	MRR     float64 `json:"mrr10"`
}

type evalBaseline struct {
	Model  string                `json:"model"`
	Groups map[string]evalScores `json:"groups"`
}

func loadCorpus(t *testing.T) evalCorpus {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "eval", "corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c evalCorpus
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var raw struct {
		Comment string `json:"comment"`
		evalCorpus
	}
	if err := dec.Decode(&raw); err != nil {
		t.Fatalf("corpus.json: %v", err)
	}
	c = raw.evalCorpus
	pages := map[string]int{}
	for _, d := range c.Docs {
		if _, dup := pages[d.Path]; dup || filepath.IsAbs(d.Path) || strings.Contains(d.Path, "..") {
			t.Fatalf("corpus: bad or duplicate path %q", d.Path)
		}
		pages[d.Path] = len(d.Pages)
	}
	answerable := 0
	for _, q := range c.Queries {
		if q.Want == "" {
			if q.Page != 0 {
				t.Fatalf("corpus: unanswerable query %q has a page", q.Q)
			}
			continue
		}
		answerable++
		n, ok := pages[q.Want]
		if !ok || q.Page < 0 || q.Page > n || (n > 0) != (q.Page > 0) {
			t.Fatalf("corpus: query %q wants %q page %d, which does not exist", q.Q, q.Want, q.Page)
		}
	}
	if answerable < 50 {
		t.Fatalf("corpus has %d answerable queries, the eval needs at least 50", answerable)
	}
	return c
}

func checkerShapes() []fixture.Shape {
	var s []fixture.Shape
	for y := range 8 {
		for x := range 8 {
			if (x+y)%2 == 0 {
				s = append(s, fixture.Shape{T: "rect", X: float64(x) / 8, Y: float64(y) / 8, W: 1. / 8, H: 1. / 8, C: "#000000"})
			}
		}
	}
	return s
}

func writeCorpus(t *testing.T, root string, c evalCorpus) {
	t.Helper()
	for _, d := range c.Docs {
		p := filepath.Join(root, d.Path)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		var err error
		switch {
		case d.Text != "":
			err = os.WriteFile(p, []byte(d.Text), 0o600)
		case len(d.Pages) > 0:
			err = fixture.PDF(p, d.Pages)
		case d.Checker:
			err = fixture.PNG(p, 1024, 768, checkerShapes())
		case len(d.Shapes) > 0:
			err = fixture.PNG(p, 1024, 768, d.Shapes)
		default:
			err = fmt.Errorf("doc has no content")
		}
		if err != nil {
			t.Fatalf("%s: %v", d.Path, err)
		}
	}
}

func startEvalSidecar(t *testing.T) *embed.Sidecar {
	t.Helper()
	bin, model := os.Getenv("SEMD_EMBED_BIN"), os.Getenv("SEMD_MODEL")
	if bin == "" || model == "" {
		t.Skip("SEMD_EMBED_BIN and SEMD_MODEL not set")
	}
	abs := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		a, err := filepath.Abs(filepath.Join("..", "..", p)) // given from the repo root
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	s, err := embed.StartSidecar(ctx, embed.SidecarConfig{
		Path: abs(bin),
		Args: []string{"--model", abs(model), "--dims", strconv.Itoa(StoreDim)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func groupOf(q evalQuery) []string {
	g := []string{"all"}
	switch {
	case q.Page > 0:
		g = append(g, "pdf")
	case kind.FromPath(q.Want) == kind.Image:
		g = append(g, "image")
	default:
		g = append(g, "text")
	}
	if q.Lang != "" {
		g = append(g, "non-en")
	}
	return g
}

func TestEval(t *testing.T) {
	sc := startEvalSidecar(t)
	defer func() {
		if err := sc.Close(); err != nil {
			t.Error(err)
		}
	}()
	c := loadCorpus(t)
	root, state := t.TempDir(), t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	writeCorpus(t, root, c)

	h := start(t, state, root, sc)
	h.wait = 10 * time.Minute
	t0 := time.Now()
	if _, err := h.e.SetRoots([]string{root}, nil); err != nil {
		t.Fatal(err)
	}
	h.idle()
	st := h.e.Status()
	t.Logf("indexed %d docs in %v", st.Indexed, time.Since(t0).Round(time.Millisecond))
	if st.Indexed != len(c.Docs) || st.Failed != 0 {
		t.Fatalf("status %+v, failures %+v", st, h.e.Failures())
	}

	type acc struct {
		n, top1, r5 int
		rr          float64
	}
	groups := map[string]*acc{}
	// Top-1 scores: when it is the relevant hit, when it is not, and for
	// queries nothing answers; the gaps say where a min-score cut could go.
	var right, wrong, none []float64
	for _, q := range c.Queries {
		hits := h.search(q.Q, evalK)
		if q.Want == "" {
			if len(hits) > 0 {
				none = append(none, float64(hits[0].Score))
				t.Logf("%-12s %-50q → %s %.3f", "unanswerable", q.Q, strings.TrimPrefix(hits[0].Path, root+"/"), hits[0].Score)
			}
			continue
		}
		rank := slices.IndexFunc(hits, func(hit Hit) bool {
			return hit.Path == filepath.Join(root, q.Want) && (q.Page == 0 || int(hit.Locator.Page) == q.Page)
		}) + 1
		top := "-"
		if len(hits) > 0 {
			top = fmt.Sprintf("%s %s %.3f", strings.TrimPrefix(hits[0].Path, root+"/"), hits[0].Locator, hits[0].Score)
			if rank == 1 {
				right = append(right, float64(hits[0].Score))
			} else {
				wrong = append(wrong, float64(hits[0].Score))
			}
		}
		mark := "ok"
		if rank != 1 {
			mark = fmt.Sprintf("MISS rank=%d", rank)
		}
		t.Logf("%-12s %-50q → %s", mark, q.Q, top)
		for _, g := range groupOf(q) {
			a := groups[g]
			if a == nil {
				a = &acc{}
				groups[g] = a
			}
			a.n++
			if rank == 1 {
				a.top1++
			}
			if rank >= 1 && rank <= 5 {
				a.r5++
			}
			if rank >= 1 {
				a.rr += 1 / float64(rank)
			}
		}
	}
	got := evalBaseline{Model: sc.Info().Model, Groups: map[string]evalScores{}}
	names := make([]string, 0, len(groups))
	for g, a := range groups {
		n := float64(a.n)
		got.Groups[g] = evalScores{N: a.n, Top1: round3(float64(a.top1) / n), Recall5: round3(float64(a.r5) / n), MRR: round3(a.rr / n)}
		names = append(names, g)
	}
	slices.Sort(names)
	for _, g := range names {
		s := got.Groups[g]
		t.Logf("%-7s n=%-3d top1=%.3f recall@5=%.3f mrr@10=%.3f", g, s.N, s.Top1, s.Recall5, s.MRR)
	}
	t.Logf("top-1 score when right: %s", spread(right))
	t.Logf("top-1 score when wrong: %s", spread(wrong))
	t.Logf("top-1 score when nothing answers: %s", spread(none))
	h.stop()

	path := filepath.Join("testdata", "eval", "baseline.json")
	if os.Getenv("SEMD_EVAL_UPDATE") == "1" {
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", path)
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no baseline (run with SEMD_EVAL_UPDATE=1 to create it): %v", err)
	}
	var base evalBaseline
	if err := json.Unmarshal(b, &base); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if base.Model != got.Model {
		t.Fatalf("baseline is for model %s, sidecar runs %s: rerun with SEMD_EVAL_UPDATE=1", base.Model, got.Model)
	}
	for g, b := range base.Groups {
		s, ok := got.Groups[g]
		switch {
		case !ok || s.N != b.N:
			t.Errorf("%s: %d queries, baseline has %d: corpus changed, update the baseline", g, s.N, b.N)
		case s.Top1 < b.Top1-evalSlack || s.Recall5 < b.Recall5-evalSlack || s.MRR < b.MRR-evalSlack:
			t.Errorf("%s regressed: %+v, baseline %+v", g, s, b)
		}
	}
}

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }

func spread(xs []float64) string {
	if len(xs) == 0 {
		return "none"
	}
	slices.Sort(xs)
	return fmt.Sprintf("n=%d min %.3f median %.3f max %.3f", len(xs), xs[0], xs[len(xs)/2], xs[len(xs)-1])
}

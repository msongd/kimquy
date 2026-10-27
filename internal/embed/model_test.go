package embed

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests against the real model. They need a built semd-embed and the
// downloaded weights, so they run only when both are named:
//
//	SEMD_EMBED_BIN=embedder/.build-xcode/Build/Products/Release/semd-embed \
//	SEMD_MODEL=models/release go test ./internal/embed -run Model -bench Model -v

const modelDims = 256 // engine.StoreDim; the sidecar truncates and re-normalizes

func startModel(tb testing.TB) *Sidecar {
	tb.Helper()
	bin, model := os.Getenv("SEMD_EMBED_BIN"), os.Getenv("SEMD_MODEL")
	if bin == "" || model == "" {
		tb.Skip("SEMD_EMBED_BIN and SEMD_MODEL not set")
	}
	abs := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		// go test runs in the package directory; paths are given from the repo root.
		a, err := filepath.Abs(filepath.Join("..", "..", p))
		if err != nil {
			tb.Fatal(err)
		}
		return a
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	t0 := time.Now()
	s, err := StartSidecar(ctx, SidecarConfig{
		Path: abs(bin),
		Args: []string{"--model", abs(model), "--dims", fmt.Sprint(modelDims)},
	})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Logf("cold start (spawn+load+hello): %v, model %s", time.Since(t0).Round(time.Millisecond), s.Info().Model)
	tb.Cleanup(func() {
		if err := s.Close(); err != nil {
			tb.Error(err)
		}
	})
	return s
}

func dot(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

// evalRow is a document with two queries that should retrieve it: one
// Russian, one English. The sets are copied from the vendored port's
// scripts/prepare_quant_eval.py, which reports q8 top-1 of 24/24 (dev) and
// 16/16 (holdout) at 768 dims.
type evalRow struct{ doc, q1, q2 string }

var evalSets = map[string][]evalRow{
	"dev": {
		{"The train departs from the north station at 08:40. Tickets include one suitcase.", "Когда отправляется поезд с северного вокзала?", "What luggage is included in the train fare?"},
		{"Return unopened headphones within fourteen days. A receipt is required for a refund.", "Можно ли вернуть нераспакованные наушники?", "What proof is needed for a headphone refund?"},
		{"The bicycle workshop replaces brake pads and straightens bent wheels on Saturdays.", "Где починить тормоза велосипеда в субботу?", "Where can I fix a bent bicycle wheel?"},
		{"Back up the database before migration. Restore the snapshot if the schema update fails.", "Как восстановить базу при неудачной миграции?", "What should happen before updating the database schema?"},
		{"Indoor basil needs sunlight and lightly moist soil. Avoid leaving water in the tray.", "Как поливать базилик на подоконнике?", "How much light does indoor basil need?"},
		{"This apartment lease allows one cat. Smoking inside and subletting are prohibited.", "Разрешает ли договор аренды держать кошку?", "Can I sublet this apartment?"},
		{"To reset the router, hold its rear button for ten seconds. Saved network settings will be erased.", "Как сбросить настройки роутера?", "What does a router factory reset erase?"},
		{"The museum is closed on Mondays. Children under six enter free with an adult.", "В какой день музей закрыт?", "Do five-year-old children need a museum ticket?"},
		{"For pancakes, mix flour, milk and eggs, then cook thin portions on a hot pan.", "Как приготовить тонкие блины?", "Which ingredients are used for pancakes?"},
		{"The parcel weighs two kilograms. It will arrive at the collection point on Friday.", "Когда посылка приедет в пункт выдачи?", "How heavy is the parcel?"},
		{"Reduce video file size by lowering the bitrate. Keep a copy of the original footage.", "Как уменьшить размер видеофайла?", "Should I preserve the original video before compression?"},
		{"The camping checklist includes a tent, sleeping bag, headlamp and drinking water.", "Что взять с собой для ночёвки в палатке?", "Which light belongs on the camping checklist?"},
	},
	"holdout": {
		{"Conference badges can be collected at the west entrance from nine in the morning.", "Где получить бейдж участника конференции?", "When does conference badge collection open?"},
		{"The printer accepts A4 paper only. Clear the rear paper path before restarting after a jam.", "Как убрать замятую бумагу из принтера?", "Which paper size does this printer accept?"},
		{"The swimming pool requires a cap. Outdoor shoes must be left in the changing room.", "Нужна ли шапочка для бассейна?", "Where should outdoor shoes be left at the pool?"},
		{"The astronomy club meets at the observatory during clear nights to view Saturn through a telescope.", "Где клуб астрономов наблюдает Сатурн?", "What weather is needed for the telescope meeting?"},
		{"Charging the wireless mouse takes ninety minutes. A blinking amber light indicates a low battery.", "Что означает мигающий жёлтый индикатор мыши?", "How long does charging the wireless mouse take?"},
		{"Store the watercolor paintings flat and away from direct sunlight to prevent the colors fading.", "Как хранить акварельные рисунки?", "Why should watercolor paintings stay out of sunlight?"},
		{"The rehearsal studio has a drum kit and two guitar amplifiers. Book at least one day in advance.", "Есть ли в репетиционной студии барабаны?", "How early should the rehearsal studio be reserved?"},
		{"The ferry carries pedestrians and bicycles but no cars. Crossings take twenty minutes.", "Можно ли перевезти автомобиль на этом пароме?", "How long is the ferry crossing?"},
	},
}

func TestModelRetrieval(t *testing.T) {
	s := startModel(t)
	ctx := context.Background()
	for _, name := range []string{"dev", "holdout"} {
		rows := evalSets[name]
		docs := make([]string, len(rows))
		var queries []string
		for i, r := range rows {
			docs[i] = DocPrompt("", r.doc)
			queries = append(queries, QueryPrompt(r.q1), QueryPrompt(r.q2))
		}
		dv, err := s.EmbedTexts(ctx, Index, docs)
		if err != nil {
			t.Fatal(err)
		}
		qv, err := s.EmbedTexts(ctx, Query, queries)
		if err != nil {
			t.Fatal(err)
		}
		correct := 0
		minMargin := math.Inf(1)
		for qi, q := range qv {
			want := qi / 2
			best, second := -1, math.Inf(-1)
			bestScore := math.Inf(-1)
			for di, d := range dv {
				sc := dot(q, d)
				if sc > bestScore {
					if best >= 0 {
						second = bestScore
					}
					best, bestScore = di, sc
				} else if sc > second {
					second = sc
				}
			}
			if best == want {
				correct++
				minMargin = min(minMargin, bestScore-second)
			} else {
				t.Errorf("%s: %q retrieved %q (%.3f) instead of %q (%.3f)", name,
					strings.TrimPrefix(queries[qi], "task: search result | query: "),
					rows[best].doc, bestScore, rows[want].doc, dot(q, dv[want]))
			}
		}
		t.Logf("%s: top-1 %d/%d at %d dims, smallest winning margin %.3f", name, correct, len(qv), modelDims, minMargin)
	}
}

// TestModelBatchInvariance checks a text gets the same vector alone, in a
// padded batch, and on a repeat call, so index-time and query-time vectors
// of identical text agree.
func TestModelBatchInvariance(t *testing.T) {
	s := startModel(t)
	ctx := context.Background()
	text := DocPrompt("", "Back up the database before migration.")
	alone, err := s.EmbedTexts(ctx, Index, []string{text})
	if err != nil {
		t.Fatal(err)
	}
	long := DocPrompt("", strings.Repeat("A much longer neighbour forces padding in the batch. ", 40))
	batch, err := s.EmbedTexts(ctx, Index, []string{long, text, long})
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.EmbedTexts(ctx, Query, []string{text})
	if err != nil {
		t.Fatal(err)
	}
	if c := dot(alone[0], batch[1]); c < 0.999 {
		t.Errorf("batched vs alone cosine %.6f, want >= .999", c)
	}
	if c := dot(alone[0], again[0]); c < 0.99999 {
		t.Errorf("repeat cosine %.7f, want >= .99999", c)
	}
}

// chunkText is roughly a 1500-character chunk, the pipeline's chunk size.
var chunkText = strings.Repeat("Quarterly revenue grew in the northern region while logistics costs fell; the board approved the new warehouse plan. ", 13)

func benchEmbed(b *testing.B, p Priority, texts []string) {
	s := startModel(b)
	ctx := context.Background()
	if _, err := s.EmbedTexts(ctx, p, texts); err != nil { // warm-up: kernels, caches
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		if _, err := s.EmbedTexts(ctx, p, texts); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(texts))*float64(b.N)/b.Elapsed().Seconds(), "texts/s")
}

func BenchmarkModelQuery(b *testing.B) {
	benchEmbed(b, Query, []string{QueryPrompt("where is the warehouse expansion plan")})
}

func BenchmarkModelIndexBatch32(b *testing.B) {
	texts := make([]string, 32)
	for i := range texts {
		texts[i] = DocPrompt("", fmt.Sprintf("%d %s", i, chunkText))
	}
	benchEmbed(b, Index, texts)
}

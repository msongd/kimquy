// Package embed is semd's view of the embedding model: the Embedder
// interface, EmbeddingGemma prompt formats, and a deterministic fake used by
// tests and by development builds without the sidecar.
package embed

import (
	"context"
	"errors"
	"hash/fnv"
	"math"
	"strings"
	"unicode"

	"github.com/msongd/kimquy/internal/kind"
)

// ErrUnavailable marks transient failures (sidecar down, restarting). Work
// that fails with it is retried, not recorded as a file failure.
var ErrUnavailable = errors.New("embedder unavailable")

// ErrInvalidInput marks input the embedder cannot handle (too long, corrupt,
// encrypted). The file is recorded as failed; retrying will not help.
var ErrInvalidInput = errors.New("embedder rejected input")

// ErrFatal marks failures that retrying cannot fix and that must stop the
// daemon: protocol violations, or a restarted model that differs from the
// one the index was built with.
var ErrFatal = errors.New("embedder fatal error")

// Priority orders requests inside the embedder; queries preempt indexing.
type Priority int

const (
	Index Priority = iota
	Query
)

// Info describes the loaded model. Model/Precision/Dims together define the
// embedding space; any change requires a reindex.
type Info struct {
	Model     string
	Precision string
	Dims      int
	Kinds     kind.Set // content kinds this embedder can handle
	MaxBatch  int
}

// Embedder turns text into vectors of Info().Dims floats.
type Embedder interface {
	Info() Info
	EmbedTexts(ctx context.Context, p Priority, texts []string) ([][]float32, error)
}

// DocPrompt is EmbeddingGemma's document prompt.
func DocPrompt(title, text string) string {
	if title == "" {
		title = "none"
	}
	return "title: " + title + " | text: " + text
}

// QueryPrompt is EmbeddingGemma's retrieval query prompt.
func QueryPrompt(q string) string { return "task: search result | query: " + q }

// Fake is a deterministic bag-of-words embedder: each word is hashed to a
// signed dimension. Texts sharing words score higher, which is enough to test
// retrieval end to end without a model.
type Fake struct {
	Dims int
}

// fakeSignalDims is the prefix Fake writes into; the store keeps 256 dims.
const fakeSignalDims = 128

func (f *Fake) Info() Info {
	return Info{Model: "fake-bow", Precision: "fp32", Dims: f.Dims, Kinds: kind.SetOf(kind.Text, kind.Office, kind.Image, kind.PDF, kind.Audio, kind.Video), MaxBatch: 64}
}

func (f *Fake) EmbedTexts(ctx context.Context, _ Priority, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out[i] = f.vector(t)
	}
	return out, nil
}

func (f *Fake) vector(t string) []float32 {
	if _, rest, ok := strings.Cut(t, " | text: "); ok && strings.HasPrefix(t, "title: ") {
		t = rest
	}
	t = strings.TrimPrefix(t, "task: search result | query: ")
	v := make([]float32, f.Dims)
	for w := range strings.FieldsFuncSeq(strings.ToLower(t), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		h := fnv.New64a()
		h.Write([]byte(w))
		x := h.Sum64()
		// Spread each word over a few dims so short texts are not too sparse.
		// Like a Matryoshka model, all signal sits in the leading dims, so
		// truncation by the store loses nothing.
		for j := range 3 {
			d := int((x >> (j * 16)) % uint64(min(f.Dims, fakeSignalDims)))
			if (x>>(48+j))&1 == 0 {
				v[d]++
			} else {
				v[d]--
			}
		}
	}
	var n float64
	for _, x := range v {
		n += float64(x * x)
	}
	if n == 0 { // no words: a fixed unit vector keeps the output valid
		v[0] = 1
		return v
	}
	inv := float32(1 / math.Sqrt(n))
	for i := range v {
		v[i] *= inv
	}
	return v
}

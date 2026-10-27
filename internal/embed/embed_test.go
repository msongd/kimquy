package embed

import (
	"context"
	"testing"
)

// The store truncates to 256 dims; the fake must keep its signal there.
func TestFakeSurvivesTruncation(t *testing.T) {
	f := &Fake{Dims: 768}
	vs, err := f.EmbedTexts(context.Background(), Index, []string{
		QueryPrompt("network backoff retry"),
		DocPrompt("net.go", "exponential backoff for flaky network calls"),
		DocPrompt("finance.md", "quarterly revenue grew"),
	})
	if err != nil {
		t.Fatal(err)
	}
	cos := func(a, b []float32) float32 {
		var s float32
		for i := range 256 {
			s += a[i] * b[i]
		}
		return s
	}
	var tail float32
	for _, x := range vs[0][256:] {
		tail += x * x
	}
	if tail != 0 {
		t.Fatalf("signal beyond dim 256: %v", tail)
	}
	if rel, unrel := cos(vs[0], vs[1]), cos(vs[0], vs[2]); rel < 0.4 || rel <= unrel {
		t.Fatalf("related %.3f, unrelated %.3f", rel, unrel)
	}
}

package chunk

import (
	"strings"
	"testing"
)

func TestSplitInvariants(t *testing.T) {
	var sb strings.Builder
	for p := range 40 {
		for l := range 5 {
			sb.WriteString(strings.Repeat("word ", 10+p%7+l))
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}
	sb.WriteString(strings.Repeat("x", 5000)) // one giant token run, no spaces
	sb.WriteString(" 日本語のテキスト")
	text := sb.String()
	chunks := Split(text, Options{Target: 64, Overlap: 8})
	if len(chunks) < 10 {
		t.Fatalf("only %d chunks", len(chunks))
	}
	prevStart, covered := -1, 0
	for i, c := range chunks {
		if c.Text != text[c.Start:c.End] {
			t.Fatalf("chunk %d text does not match its range", i)
		}
		if c.Start <= prevStart {
			t.Fatalf("chunk %d does not advance: start %d prev %d", i, c.Start, prevStart)
		}
		if c.Start > covered {
			t.Fatalf("gap before chunk %d: [%d,%d)", i, covered, c.Start)
		}
		if tok := EstimateTokens(c.Text); tok > 64 {
			t.Fatalf("chunk %d has %d tokens > target", i, tok)
		}
		prevStart, covered = c.Start, c.End
	}
	if covered != len(text) {
		t.Fatalf("text not fully covered: %d of %d", covered, len(text))
	}
}

func TestSplitEdgeCases(t *testing.T) {
	if c := Split("   \n\n\t", Options{}); len(c) != 0 {
		t.Fatalf("whitespace produced chunks: %+v", c)
	}
	c := Split("hello world", Options{})
	if len(c) != 1 || c[0].Text != "hello world" {
		t.Fatalf("short text: %+v", c)
	}
}

func FuzzSplit(f *testing.F) {
	f.Add("a b c\n\nd e f", 4)
	f.Add(strings.Repeat("é", 300), 3)
	f.Fuzz(func(t *testing.T, text string, target int) {
		target = 1 + (target&0xff)%128
		for _, c := range Split(text, Options{Target: target}) {
			if c.Start < 0 || c.End > len(text) || c.Start >= c.End || c.Text != text[c.Start:c.End] {
				t.Fatalf("bad chunk %+v", c)
			}
		}
	})
}

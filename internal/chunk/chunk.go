// Package chunk splits extracted text into overlapping, roughly token-sized
// windows that break at paragraph, line or word boundaries.
//
// Token counts are estimated (≈4 bytes per token for Latin text, one token per
// CJK rune), which is close enough for EmbeddingGemma's SentencePiece
// vocabulary to stay well inside its context window.
package chunk

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Chunk is a window of the source text; Text == src[Start:End].
type Chunk struct {
	Start, End int
	Text       string
}

// Options control chunk size. Zero values pick defaults.
type Options struct {
	Target  int // tokens per chunk (default 256)
	Overlap int // tokens repeated from the previous chunk (default 32)
}

// EstimateTokens approximates the tokenizer's count for s.
func EstimateTokens(s string) int {
	n, latin := 0, 0
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			n += (latin + 3) / 4
			latin = 0
		case r >= 0x2E80: // CJK and beyond: roughly one token per rune
			n += (latin+3)/4 + 1
			latin = 0
		default:
			latin += utf8.RuneLen(r)
		}
	}
	return n + (latin+3)/4
}

type piece struct {
	start, end int
	tokens     int
	paraStart  bool // first piece after a blank line
}

// Split chunks text. Whitespace-only text yields no chunks.
func Split(text string, opt Options) []Chunk {
	if opt.Target <= 0 {
		opt.Target = 256
	}
	if opt.Overlap < 0 || opt.Overlap >= opt.Target {
		opt.Overlap = 0
	} else if opt.Overlap == 0 {
		opt.Overlap = min(32, opt.Target/8)
	}
	pieces := split(text, opt.Target)

	var out []Chunk
	for i := 0; i < len(pieces); {
		j, tok := i, 0
		for j < len(pieces) {
			p := pieces[j]
			if j > i && tok+p.tokens > opt.Target {
				break
			}
			// Prefer to end at a paragraph once the chunk is half full.
			if j > i && p.paraStart && tok >= opt.Target/2 {
				break
			}
			tok += p.tokens
			j++
		}
		c := Chunk{Start: pieces[i].start, End: pieces[j-1].end}
		c.Text = text[c.Start:c.End]
		if strings.TrimSpace(c.Text) != "" {
			out = append(out, c)
		}
		if j == len(pieces) {
			break
		}
		// Back up over trailing pieces worth ≤ Overlap tokens, but always advance.
		k, back := j, 0
		for k-1 > i && back+pieces[k-1].tokens <= opt.Overlap {
			k--
			back += pieces[k].tokens
		}
		i = k
	}
	return out
}

// split cuts text into lines, and lines longer than limit tokens into words
// (and words longer than limit into rune runs).
func split(text string, limit int) []piece {
	var ps []piece
	blank := true
	for off := 0; off < len(text); {
		end := strings.IndexByte(text[off:], '\n')
		if end < 0 {
			end = len(text)
		} else {
			end += off + 1
		}
		line := text[off:end]
		isBlank := strings.TrimSpace(line) == ""
		if t := EstimateTokens(line); t <= limit {
			ps = append(ps, piece{off, end, t, blank && !isBlank})
		} else {
			first := true
			for _, w := range words(line, off, limit) {
				w.paraStart = first && blank
				first = false
				ps = append(ps, w)
			}
		}
		blank = isBlank
		off = end
	}
	return ps
}

func words(line string, base, limit int) []piece {
	var ps []piece
	start := 0
	emit := func(end int) {
		if end <= start {
			return
		}
		seg := line[start:end]
		if t := EstimateTokens(seg); t <= limit {
			ps = append(ps, piece{start: base + start, end: base + end, tokens: t})
		} else { // a single huge "word": cut by bytes on rune boundaries
			step := limit * 4
			for s := start; s < end; {
				e := min(end, s+step)
				for e < end && !utf8.RuneStart(line[e]) {
					e++
				}
				ps = append(ps, piece{start: base + s, end: base + e, tokens: EstimateTokens(line[s:e])})
				s = e
			}
		}
		start = end
	}
	inSpace := false
	for i, r := range line {
		sp := unicode.IsSpace(r)
		if inSpace && !sp { // a word ends after its trailing whitespace
			emit(i)
		}
		inSpace = sp
	}
	emit(len(line))
	return ps
}

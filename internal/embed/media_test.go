package embed

import (
	"strings"
	"testing"
)

func TestPDFPagesCheck(t *testing.T) {
	vec := make([]float32, 4)
	req := PDFRequest{Path: "/x.pdf", From: 3, Count: 2, Render: 1}
	ok := PDFPages{PageCount: 4, Pages: []PDFPage{{Page: 3, Vec: vec}, {Page: 4, Text: "t"}}}
	if err := ok.check(req, 4); err != nil {
		t.Fatalf("valid reply rejected: %v", err)
	}
	// Past the end: no pages is the right answer.
	if err := (PDFPages{PageCount: 2}).check(req, 4); err != nil {
		t.Fatalf("empty tail rejected: %v", err)
	}
	bad := map[string]PDFPages{
		"no pages":       {PageCount: 0},
		"too few":        {PageCount: 4, Pages: ok.Pages[:1]},
		"out of order":   {PageCount: 4, Pages: []PDFPage{ok.Pages[1], ok.Pages[0]}},
		"over budget":    {PageCount: 4, Pages: []PDFPage{{Page: 3, Vec: vec}, {Page: 4, Vec: vec}}},
		"wrong dims":     {PageCount: 4, Pages: []PDFPage{{Page: 3, Vec: vec[:3]}, {Page: 4}}},
		"both":           {PageCount: 4, Pages: []PDFPage{{Page: 3, Vec: vec, Unrendered: true}, {Page: 4}}},
		"not utf8":       {PageCount: 4, Pages: []PDFPage{{Page: 3, Text: "\xff"}, {Page: 4}}},
		"text over cap":  {PageCount: 4, Pages: []PDFPage{{Page: 3, Text: strings.Repeat("a", MaxPDFPageText+1)}, {Page: 4}}},
		"too many pages": {PageCount: 1 << 16, Pages: ok.Pages},
	}
	for name, r := range bad {
		if err := r.check(req, 4); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPDFRequestValidate(t *testing.T) {
	for _, r := range []PDFRequest{
		{Path: "rel.pdf", From: 1, Count: 1},
		{Path: "/a.pdf", From: 0, Count: 1},
		{Path: "/a.pdf", From: 1, Count: 0},
		{Path: "/a.pdf", From: 1, Count: MaxPDFPagesPerCall + 1},
		{Path: "/a.pdf", From: 1, Count: 2, Render: 3},
		{Path: "/a.pdf", From: 1, Count: 2, Render: -1},
	} {
		if r.validate() == nil {
			t.Errorf("%+v accepted", r)
		}
	}
}

func TestTooLittleText(t *testing.T) {
	if !TooLittleText("  Figure 3 \n\t ") {
		t.Error("short text not flagged")
	}
	if TooLittleText(strings.Repeat("я", PDFRenderMinChars)) {
		t.Error("enough text flagged")
	}
}

package embed

import (
	"bytes"
	"image"
	"image/png"
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

func TestPDFPicture(t *testing.T) {
	enc := func(w, h int) []byte {
		var b bytes.Buffer
		if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	req := PDFPictureRequest{Path: "/a.pdf", Page: 2}
	ok := PDFPicture{PageCount: 3, PNG: enc(30, 40)}
	if err := ok.check(req); err != nil || ok.Width != 30 || ok.Height != 40 {
		t.Fatalf("valid reply: %v %d×%d", err, ok.Width, ok.Height)
	}
	for name, r := range map[string]PDFPicture{
		"no pages":  {PageCount: 0, PNG: enc(1, 1)},
		"past end":  {PageCount: 1, PNG: enc(1, 1)},
		"empty":     {PageCount: 3},
		"not png":   {PageCount: 3, PNG: []byte("GIF89a")},
		"too wide":  {PageCount: 3, PNG: enc(PDFPictureMaxSide+1, 2)},
		"too large": {PageCount: 3, PNG: make([]byte, MaxPDFPicture+1)},
	} {
		if err := r.check(req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for _, r := range []PDFPictureRequest{
		{Path: "rel.pdf", Page: 1},
		{Path: "/a.pdf", Page: 0},
		{Path: "/a.pdf", Page: 1, MatchStart: 5, MatchEnd: 2},
		{Path: "/a.pdf", Page: 1, MatchStart: -1},
		{Path: "/a.pdf", Page: 1, MatchEnd: MaxPDFPageText + 1},
	} {
		if r.validate() == nil {
			t.Errorf("%+v accepted", r)
		}
	}
}

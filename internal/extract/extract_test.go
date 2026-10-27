package extract

import (
	"archive/zip"
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestText(t *testing.T) {
	for _, tc := range []struct {
		in   []byte
		want string
	}{
		{[]byte("plain"), "plain"},
		{[]byte("\xef\xbb\xbfbom"), "bom"},
		{[]byte{0xff, 0xfe, 'h', 0, 'i', 0}, "hi"},
		{[]byte{0xfe, 0xff, 0, 'h', 0, 'i'}, "hi"},
	} {
		got, err := Text(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("Text(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range [][]byte{{'a', 0, 'b'}, {0xc3, 0x28}} {
		if _, err := Text(bad); !errors.Is(err, ErrNotText) {
			t.Errorf("Text(%q) err = %v, want ErrNotText", bad, err)
		}
	}
	if !LooksLikeText([]byte("héllo")[:2]) || LooksLikeText([]byte{0x7f, 'E', 'L', 'F', 0}) {
		t.Error("LooksLikeText misclassified")
	}
}

func makeZip(t *testing.T, parts map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range parts {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	return buf.Bytes()
}

func TestOffice(t *testing.T) {
	docx := makeZip(t, map[string]string{"word/document.xml": `<w:document xmlns:w="x"><w:body>
		<w:p><w:r><w:t>Hello</w:t></w:r><w:r><w:tab/><w:t xml:space="preserve"> world</w:t></w:r></w:p>
		<w:p><w:r><w:t>Second para</w:t></w:r></w:p></w:body></w:document>`})
	got, err := Office(docx, ".docx")
	if err != nil || got != "Hello\t world\nSecond para\n" {
		t.Fatalf("docx = %q, %v", got, err)
	}
	pptx := makeZip(t, map[string]string{
		"ppt/slides/slide10.xml": `<p:sld xmlns:a="a" xmlns:p="p"><a:p><a:r><a:t>ten</a:t></a:r></a:p></p:sld>`,
		"ppt/slides/slide2.xml":  `<p:sld xmlns:a="a" xmlns:p="p"><a:p><a:r><a:t>two</a:t></a:r></a:p></p:sld>`,
	})
	got, err = Office(pptx, ".pptx")
	if err != nil || got != "two\n\nten\n\n" {
		t.Fatalf("pptx = %q, %v", got, err)
	}
	xlsx := makeZip(t, map[string]string{
		"xl/sharedStrings.xml":     `<sst><si><t>Revenue</t></si><si><t>Q3</t></si></sst>`,
		"xl/worksheets/sheet1.xml": `<worksheet><c t="inlineStr"><is><t>inline</t></is></c></worksheet>`,
	})
	got, err = Office(xlsx, ".xlsx")
	if err != nil || !strings.Contains(got, "Revenue\nQ3\n") || !strings.Contains(got, "inline") {
		t.Fatalf("xlsx = %q, %v", got, err)
	}
	if _, err := Office([]byte("not a zip"), ".docx"); err == nil {
		t.Fatal("garbage accepted")
	}
	if _, err := Office(makeZip(t, map[string]string{"x": "y"}), ".docx"); err == nil {
		t.Fatal("docx without document.xml accepted")
	}
}

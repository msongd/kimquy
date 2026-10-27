// Package extract turns file bytes into plain UTF-8 text for the kinds that
// are handled in Go: plain text/code and Office Open XML (docx, xlsx, pptx).
package extract

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// ErrNotText means the content is binary or not decodable as text.
var ErrNotText = errors.New("not a text file")

// maxOfficeXML caps the decompressed XML read from one Office document, as a
// zip-bomb guard.
const maxOfficeXML = 64 << 20

// LooksLikeText sniffs the first bytes of a file with no known extension.
func LooksLikeText(head []byte) bool {
	if bytes.HasPrefix(head, []byte{0xff, 0xfe}) || bytes.HasPrefix(head, []byte{0xfe, 0xff}) {
		return true
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return false
	}
	// The sample may end mid-rune; ignore up to 3 trailing bytes.
	for cut := 0; cut <= 3 && cut <= len(head); cut++ {
		if utf8.Valid(head[:len(head)-cut]) {
			return true
		}
	}
	return false
}

// Text decodes a text file: UTF-8 (BOM stripped) or BOM-marked UTF-16.
func Text(b []byte) (string, error) {
	switch {
	case bytes.HasPrefix(b, []byte{0xef, 0xbb, 0xbf}):
		b = b[3:]
	case bytes.HasPrefix(b, []byte{0xff, 0xfe}):
		return decodeUTF16(b[2:], false)
	case bytes.HasPrefix(b, []byte{0xfe, 0xff}):
		return decodeUTF16(b[2:], true)
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return "", fmt.Errorf("%w: contains NUL bytes", ErrNotText)
	}
	if !utf8.Valid(b) {
		return "", fmt.Errorf("%w: invalid UTF-8", ErrNotText)
	}
	return string(b), nil
}

func decodeUTF16(b []byte, bigEndian bool) (string, error) {
	if len(b)%2 != 0 {
		return "", fmt.Errorf("%w: odd-length UTF-16", ErrNotText)
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		if bigEndian {
			u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
		} else {
			u[i] = uint16(b[2*i+1])<<8 | uint16(b[2*i])
		}
	}
	return string(utf16.Decode(u)), nil
}

// Office extracts text from a docx, xlsx or pptx file, chosen by ext.
func Office(b []byte, ext string) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return "", fmt.Errorf("office: %w", err)
	}
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	budget := int64(maxOfficeXML)
	read := func(name string) ([]byte, error) {
		f, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("office: missing part %s", name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("office: %s: %w", name, err)
		}
		defer rc.Close()
		data, err := io.ReadAll(io.LimitReader(rc, budget+1))
		if err != nil {
			return nil, fmt.Errorf("office: %s: %w", name, err)
		}
		budget -= int64(len(data))
		if budget < 0 {
			return nil, fmt.Errorf("office: decompressed XML exceeds %d bytes", maxOfficeXML)
		}
		return data, nil
	}

	var sb strings.Builder
	switch strings.ToLower(ext) {
	case ".docx":
		x, err := read("word/document.xml")
		if err != nil {
			return "", err
		}
		err = xmlText(&sb, x, "t", map[string]string{"p": "\n", "tab": "\t", "br": "\n"})
		return sb.String(), err
	case ".pptx":
		for _, name := range numbered(files, "ppt/slides/slide", ".xml") {
			x, err := read(name)
			if err != nil {
				return "", err
			}
			if err := xmlText(&sb, x, "t", map[string]string{"p": "\n"}); err != nil {
				return "", err
			}
			sb.WriteString("\n")
		}
		return sb.String(), nil
	case ".xlsx":
		if _, ok := files["xl/sharedStrings.xml"]; ok {
			x, err := read("xl/sharedStrings.xml")
			if err != nil {
				return "", err
			}
			if err := xmlText(&sb, x, "t", map[string]string{"si": "\n"}); err != nil {
				return "", err
			}
		}
		// Inline strings live in the sheets themselves.
		for _, name := range numbered(files, "xl/worksheets/sheet", ".xml") {
			x, err := read(name)
			if err != nil {
				return "", err
			}
			if err := xmlText(&sb, x, "t", map[string]string{"is": "\n"}); err != nil {
				return "", err
			}
		}
		return sb.String(), nil
	}
	return "", fmt.Errorf("office: unsupported extension %q", ext)
}

// numbered returns zip parts named prefix<N>suffix sorted by N.
func numbered(files map[string]*zip.File, prefix, suffix string) []string {
	type part struct {
		n    int
		name string
	}
	var parts []part
	for name := range files {
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) || path.Dir(name) != path.Dir(prefix+"x") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix))
		if err == nil {
			parts = append(parts, part{n, name})
		}
	}
	slices.SortFunc(parts, func(a, b part) int { return a.n - b.n })
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = p.name
	}
	return out
}

// xmlText writes the character data of every element with local name textTag,
// and emits seps[local] when such an element ends (or, for "tab"/"br", starts).
func xmlText(sb *strings.Builder, x []byte, textTag string, seps map[string]string) error {
	d := xml.NewDecoder(bytes.NewReader(x))
	depth := 0 // > 0 while inside a text element
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("office: xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == textTag {
				depth++
			} else if s, ok := seps[t.Name.Local]; ok && (t.Name.Local == "tab" || t.Name.Local == "br") {
				sb.WriteString(s)
			}
		case xml.EndElement:
			if t.Name.Local == textTag {
				depth--
			} else if s, ok := seps[t.Name.Local]; ok && t.Name.Local != "tab" && t.Name.Local != "br" {
				sb.WriteString(s)
			}
		case xml.CharData:
			if depth > 0 {
				sb.Write(t)
			}
		}
	}
}

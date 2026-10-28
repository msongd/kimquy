// Package kind defines the content kinds semd indexes and how indexed
// fragments are located inside their source file.
package kind

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// Kind is the content class of a file or of an indexed fragment.
type Kind uint8

const (
	Unknown Kind = iota
	Text
	Office
	PDF
	Image
	Audio
	Video
	numKinds
)

var names = [numKinds]string{"unknown", "text", "office", "pdf", "image", "audio", "video"}

func (k Kind) String() string {
	if k >= numKinds {
		return fmt.Sprintf("kind(%d)", uint8(k))
	}
	return names[k]
}

// Valid reports whether k is a known, indexable kind.
func (k Kind) Valid() bool { return k > Unknown && k < numKinds }

// Parse maps a kind name back to its Kind.
func Parse(s string) (Kind, error) {
	for i, n := range names {
		if n == s && Kind(i).Valid() {
			return Kind(i), nil
		}
	}
	return Unknown, fmt.Errorf("unknown kind %q", s)
}

// Set is a bitmask of kinds. The zero Set matches every kind.
type Set uint16

func SetOf(ks ...Kind) Set {
	var s Set
	for _, k := range ks {
		s |= 1 << k
	}
	return s
}

func (s Set) Has(k Kind) bool { return s == 0 || s&(1<<k) != 0 }

var textExt = map[string]bool{
	".txt": true, ".md": true, ".markdown": true, ".rst": true, ".org": true, ".csv": true, ".tsv": true,
	".log": true, ".json": true, ".yaml": true, ".yml": true, ".toml": true, ".ini": true, ".xml": true,
	".html": true, ".htm": true, ".tex": true, ".go": true, ".py": true, ".js": true, ".ts": true,
	".jsx": true, ".tsx": true, ".java": true, ".kt": true, ".scala": true, ".c": true, ".h": true,
	".cc": true, ".cpp": true, ".hpp": true, ".m": true, ".mm": true, ".swift": true, ".rs": true,
	".rb": true, ".php": true, ".sh": true, ".zsh": true, ".bash": true, ".sql": true, ".css": true,
	".scss": true, ".lua": true, ".r": true, ".pl": true, ".cs": true, ".fs": true, ".hs": true,
	".ex": true, ".exs": true, ".erl": true, ".clj": true, ".vue": true, ".svelte": true, ".proto": true,
}

var extKind = map[string]Kind{
	".docx": Office, ".xlsx": Office, ".pptx": Office,
	".pdf": PDF,
	".jpg": Image, ".jpeg": Image, ".png": Image, ".gif": Image, ".heic": Image, ".heif": Image,
	".webp": Image, ".tif": Image, ".tiff": Image, ".bmp": Image,
	".mp3": Audio, ".m4a": Audio, ".wav": Audio, ".aiff": Audio, ".aif": Audio, ".flac": Audio,
	".aac": Audio, ".caf": Audio,
	".mp4": Video, ".mov": Video, ".m4v": Video,
}

// FromPath guesses the kind from the file extension. Files without a known
// extension return Unknown; callers may sniff content to decide if they are text.
func FromPath(path string) Kind {
	ext := strings.ToLower(filepath.Ext(path))
	if textExt[ext] {
		return Text
	}
	return extKind[ext]
}

// LocKind says how a Locator addresses a fragment.
type LocKind uint8

const (
	LocNone  LocKind = iota
	LocBytes         // [Start, End) byte range in extracted text
	LocPage          // Page (1-based); [Start, End) byte range in that page's text, or 0-0 for the page as an image
	LocTime          // [Start, End) milliseconds
)

// MaxPage is the highest page number a Locator can hold.
const MaxPage = 1<<16 - 1

// Locator points at the fragment of a file that a vector represents.
type Locator struct {
	Kind       LocKind
	Page       uint16
	Start, End int64
}

// Validate checks that the fields make sense for the locator's kind.
func (l Locator) Validate() error {
	switch l.Kind {
	case LocNone:
		if l.Page != 0 || l.Start != 0 || l.End != 0 {
			return fmt.Errorf("locator none with fields set: %+v", l)
		}
	case LocBytes, LocTime:
		if l.Page != 0 || l.Start < 0 || l.End <= l.Start {
			return fmt.Errorf("locator %s: bad range", l)
		}
	case LocPage:
		if l.Page == 0 || l.Start < 0 || l.End < l.Start || (l.End == l.Start && l.Start != 0) {
			return fmt.Errorf("locator %s: bad page or range", l)
		}
	default:
		return fmt.Errorf("locator: unknown kind %d", l.Kind)
	}
	return nil
}

func (l Locator) String() string {
	switch l.Kind {
	case LocBytes:
		return fmt.Sprintf("bytes:%d-%d", l.Start, l.End)
	case LocPage:
		if l.End == 0 {
			return fmt.Sprintf("page:%d", l.Page)
		}
		return fmt.Sprintf("page:%d:%d-%d", l.Page, l.Start, l.End)
	case LocTime:
		return fmt.Sprintf("time:%d-%dms", l.Start, l.End)
	}
	return "none"
}

// ParseLocator reads a locator as String writes it, e.g. from semctl
// search's output.
func ParseLocator(s string) (Locator, error) {
	var l Locator
	bad := func() (Locator, error) { return Locator{}, fmt.Errorf("bad locator %q", s) }
	typ, rest, _ := strings.Cut(s, ":")
	var err error
	switch typ {
	case "none":
		if rest != "" {
			return bad()
		}
		return l, nil
	case "bytes":
		l.Kind = LocBytes
		_, err = fmt.Sscanf(rest, "%d-%d", &l.Start, &l.End)
	case "time":
		l.Kind = LocTime
		rest, ok := strings.CutSuffix(rest, "ms")
		if !ok {
			return bad()
		}
		_, err = fmt.Sscanf(rest, "%d-%d", &l.Start, &l.End)
	case "page":
		l.Kind = LocPage
		page, rng, hasRange := strings.Cut(rest, ":")
		var n int
		if _, err = fmt.Sscanf(page, "%d", &n); err == nil && (n < 1 || n > MaxPage) {
			return bad()
		}
		l.Page = uint16(n)
		if err == nil && hasRange {
			_, err = fmt.Sscanf(rng, "%d-%d", &l.Start, &l.End)
		}
	default:
		return bad()
	}
	if err != nil || l.String() != s {
		return bad() // trailing text, or a form String does not write
	}
	if err := l.Validate(); err != nil {
		return Locator{}, err
	}
	return l, nil
}

var locNames = map[LocKind]string{LocNone: "none", LocBytes: "bytes", LocPage: "page", LocTime: "time"}

type locJSON struct {
	Type  string `json:"type"`
	Page  uint16 `json:"page,omitempty"`
	Start int64  `json:"start"`
	End   int64  `json:"end"`
}

func (l Locator) MarshalJSON() ([]byte, error) {
	n, ok := locNames[l.Kind]
	if !ok {
		return nil, fmt.Errorf("locator: unknown kind %d", l.Kind)
	}
	return json.Marshal(locJSON{n, l.Page, l.Start, l.End})
}

func (l *Locator) UnmarshalJSON(b []byte) error {
	var j locJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	for k, n := range locNames {
		if n == j.Type {
			*l = Locator{k, j.Page, j.Start, j.End}
			return nil
		}
	}
	return fmt.Errorf("locator: unknown type %q", j.Type)
}

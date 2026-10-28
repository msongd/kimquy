package kind

import "testing"

func TestParseLocator(t *testing.T) {
	for _, l := range []Locator{
		{},
		{Kind: LocBytes, Start: 3, End: 90},
		{Kind: LocPage, Page: 2},
		{Kind: LocPage, Page: 7, Start: 0, End: 120},
		{Kind: LocTime, Start: 1000, End: 31000},
	} {
		got, err := ParseLocator(l.String())
		if err != nil || got != l {
			t.Errorf("ParseLocator(%q) = %+v, %v; want %+v", l, got, err, l)
		}
	}
	for _, s := range []string{
		"", "none:1", "bytes", "bytes:5-2", "bytes:1-2x", "bytes:+1-2", "page:0", "page:70000",
		"page:3:", "page:3:4-2", "time:1-2", "time:2-1ms", "words:1-2",
	} {
		if l, err := ParseLocator(s); err == nil {
			t.Errorf("ParseLocator(%q) = %+v, want an error", s, l)
		}
	}
}

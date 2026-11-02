package config

import (
	"path/filepath"
	"testing"
)

func TestDefaultExcludes(t *testing.T) {
	for _, p := range DefaultExcludes {
		if _, err := filepath.Match(p, ""); err != nil {
			t.Fatalf("default exclude %q: %v", p, err)
		}
	}
	c := Default()
	root := "/r"
	for path, want := range map[string]bool{
		"/r/a/temp/c-1.txt":         true,
		"/r/temp":                   true,
		"/r/temperature.txt":        false,
		"/r/a/server.log":           true,
		"/r/a/logs/x.txt":           false,
		"/r/js/ts.worker.min.js":    true,
		"/r/js/app.js":              false,
		"/r/a/node_modules/x/i.js":  true,
		"/r/.git/config":            true,
		"/r/notes/minutes.min.json": false,
	} {
		if got := c.ExcludedPath(root, path); got != want {
			t.Errorf("ExcludedPath(%q) = %v, want %v", path, got, want)
		}
	}
}

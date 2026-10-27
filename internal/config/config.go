// Package config holds semd's user settings: which directories to index, what
// to skip, and resource limits. It is a JSON file validated at load; an
// invalid file is an error, never silently replaced with defaults.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/msongd/kimquy/internal/fsutil"
	"github.com/msongd/kimquy/internal/kind"
)

// Limits caps the input size per kind, in bytes. 0 means unlimited.
type Limits struct {
	Text  int64 `json:"text"`
	PDF   int64 `json:"pdf"`
	Image int64 `json:"image"`
}

// Config is the full user configuration.
type Config struct {
	Roots          []string `json:"roots"`
	Excludes       []string `json:"excludes"` // globs; with a "/" they match the root-relative path, otherwise the base name
	Limits         Limits   `json:"limits"`
	Workers        int      `json:"workers"`
	PauseOnBattery bool     `json:"pause_on_battery"`
}

// DefaultExcludes are always applied in addition to Config.Excludes.
var DefaultExcludes = []string{".*", "node_modules", "__pycache__", "*.tmp", "*~"}

// Default returns the configuration used when no file exists yet.
func Default() Config {
	return Config{
		Roots:          []string{},
		Excludes:       []string{},
		Limits:         Limits{Text: 10 << 20, PDF: 200 << 20, Image: 50 << 20},
		Workers:        1,
		PauseOnBattery: true,
	}
}

// Validate checks c and normalizes roots in place (clean, deduplicated, sorted).
func (c *Config) Validate() error {
	if c.Workers < 1 || c.Workers > 64 {
		return fmt.Errorf("config: workers %d out of [1,64]", c.Workers)
	}
	if c.Limits.Text < 0 || c.Limits.PDF < 0 || c.Limits.Image < 0 {
		return errors.New("config: limits must be ≥ 0")
	}
	roots := make([]string, 0, len(c.Roots))
	for _, r := range c.Roots {
		if !filepath.IsAbs(r) {
			return fmt.Errorf("config: root %q is not absolute", r)
		}
		roots = append(roots, filepath.Clean(r))
	}
	slices.Sort(roots)
	c.Roots = slices.Compact(roots)
	for _, p := range c.Excludes {
		if p == "" {
			return errors.New("config: empty exclude pattern")
		}
		if _, err := filepath.Match(p, "x"); err != nil {
			return fmt.Errorf("config: exclude %q: %w", p, err)
		}
	}
	return nil
}

// Load reads path; a missing file yields Default().
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	c := Default()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Save validates c and writes it atomically.
func Save(path string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, append(b, '\n'))
}

// Limit returns the size cap for k (0 = none).
func (c *Config) Limit(k kind.Kind) int64 {
	switch k {
	case kind.Text, kind.Office:
		return c.Limits.Text
	case kind.PDF:
		return c.Limits.PDF
	case kind.Image:
		return c.Limits.Image
	}
	return 0
}

// RootOf returns the configured root containing path, if any. With nested
// roots, the innermost one wins.
func (c *Config) RootOf(path string) (string, bool) {
	best := ""
	for _, r := range c.Roots {
		if Within(r, path) && len(r) > len(best) {
			best = r
		}
	}
	return best, best != ""
}

// Within reports whether path is root or lies below it.
func Within(root, path string) bool {
	if path == root {
		return true
	}
	if root == "/" {
		return strings.HasPrefix(path, "/")
	}
	return strings.HasPrefix(path, root) && path[len(root)] == filepath.Separator
}

// Excluded reports whether the entry at path (under root) should be skipped.
func (c *Config) Excluded(root, path string) bool {
	base := filepath.Base(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	for _, pats := range [][]string{DefaultExcludes, c.Excludes} {
		for _, p := range pats {
			target := base
			if strings.ContainsRune(p, '/') {
				target = rel
			}
			if ok, _ := filepath.Match(p, target); ok { // patterns validated at load
				return true
			}
		}
	}
	return false
}

// ExcludedPath reports whether path, or any directory between it and root,
// is excluded.
func (c *Config) ExcludedPath(root, path string) bool {
	for p := path; p != root && len(p) > len(root); p = filepath.Dir(p) {
		if c.Excluded(root, p) {
			return true
		}
	}
	return false
}

// DefaultStateDir is where semd keeps config, catalog, store and its socket:
// ~/Library/Application Support/Kim Quy on macOS.
func DefaultStateDir() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "Kim Quy"), nil
}

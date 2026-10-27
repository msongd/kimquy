// Package crawl walks the configured roots and reports candidate files.
// Symlinks are never followed, which rules out loops and escaping a root.
package crawl

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"

	"github.com/msongd/kimquy/internal/config"
	"github.com/msongd/kimquy/internal/kind"
)

// Visitor receives events from Walk. Returning an error from File aborts the walk.
type Visitor struct {
	// File is called for each regular, non-excluded file whose kind is in
	// kinds, or that has no extension (the caller sniffs it).
	File func(root, path string, info fs.FileInfo) error
	// Problem reports a path that could not be read. For a root this means
	// none of it was scanned; the caller must not treat its files as deleted.
	Problem func(root, path string, err error)
}

// Walk scans every root in cfg or, when under is not "", only what lies
// below under: part of one root, or every root inside it.
func Walk(ctx context.Context, cfg *config.Config, kinds kind.Set, under string, v Visitor) error {
	for _, root := range cfg.Roots {
		start := root
		switch {
		case under == "" || config.Within(under, root):
		case config.Within(root, under):
			// A nested root owns its own subtree.
			if owner, _ := cfg.RootOf(under); owner != root || cfg.ExcludedPath(root, under) {
				continue
			}
			start = under
		default:
			continue
		}
		if err := walk(ctx, cfg, kinds, root, start, v); err != nil {
			return err
		}
	}
	return nil
}

func walk(ctx context.Context, cfg *config.Config, kinds kind.Set, root, start string, v Visitor) error {
	return filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			if p == start && p != root && errors.Is(err, fs.ErrNotExist) {
				return nil // the subtree went away; the caller handles removals
			}
			v.Problem(root, p, err)
			if p == root {
				return filepath.SkipDir
			}
			return nil // WalkDir skips the unreadable directory itself
		}
		if p == root && !d.IsDir() {
			v.Problem(root, p, errors.New("root is not a directory (give symlinked roots by their real path)"))
			return filepath.SkipDir
		}
		if p != root {
			if cfg.Excluded(root, p) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			// A nested root is walked on its own; skip it here so its
			// files belong to (and are excluded by) the innermost root.
			if d.IsDir() && isRoot(cfg, p) {
				return filepath.SkipDir
			}
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		k := kind.FromPath(p)
		if k == kind.Unknown && filepath.Ext(p) != "" {
			return nil
		}
		if k != kind.Unknown && !kinds.Has(k) {
			return nil
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil // deleted mid-walk
		}
		if err != nil {
			v.Problem(root, p, err)
			return nil
		}
		return v.File(root, p, info)
	})
}

func isRoot(cfg *config.Config, p string) bool {
	for _, r := range cfg.Roots {
		if r == p {
			return true
		}
	}
	return false
}

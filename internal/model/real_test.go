package model

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRealDownload fetches text-q8 from Hugging Face into
// $KIMQUY_REAL_DOWNLOAD/models (320 MB), stopping part way once and
// resuming. It is skipped unless that is set.
func TestRealDownload(t *testing.T) {
	root := os.Getenv("KIMQUY_REAL_DOWNLOAD")
	if root == "" {
		t.Skip("KIMQUY_REAL_DOWNLOAD unset")
	}
	in := NewInstaller(filepath.Join(root, "models"))
	ctx, cancel := context.WithCancel(context.Background())
	var stopped int64
	err := in.Download(ctx, Parts[:1], func(done, _ int64) {
		if done > 100<<20 && stopped == 0 {
			stopped = done
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("first try: %v", err)
	}
	t.Logf("stopped at %s", Bytes(stopped))
	var first int64 = -1
	if err := in.Download(context.Background(), Parts[:1], func(done, _ int64) {
		if first < 0 {
			first = done
		}
	}); err != nil {
		t.Fatal(err)
	}
	if first < 100<<20 {
		t.Errorf("resumed from %s, not where it stopped", Bytes(first))
	}
	if err := Verify(in.Dir, Parts[0], nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("resumed from %s; verified", Bytes(first))
}

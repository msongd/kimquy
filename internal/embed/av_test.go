package embed

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/msongd/kimquy/internal/kind"
)

func TestAudioRequestValidate(t *testing.T) {
	ok := AudioRequest{Path: "/a.mp3", StartMS: 30_000, EndMS: 60_000}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	for _, r := range []AudioRequest{
		{Path: "a.mp3", StartMS: 0, EndMS: 1000},
		{Path: "/a.mp3", StartMS: -1, EndMS: 1000},
		{Path: "/a.mp3", StartMS: 0, EndMS: MinAudioWindowMS - 1},
		{Path: "/a.mp3", StartMS: 0, EndMS: MaxAudioWindowMS + 1},
		{Path: "/a.mp3", StartMS: 1000, EndMS: 0},
		{Path: "/a.mp3", StartMS: MaxMediaMS, EndMS: MaxMediaMS + 1000},
	} {
		if r.validate() == nil {
			t.Errorf("accepted %+v", r)
		}
	}
	if validateFrames("/v.mp4", []int64{0, 5000}) != nil {
		t.Fatal("rejected good frames")
	}
	for _, ts := range [][]int64{nil, make([]int64, MaxFramesPerCall+1), {-1}, {MaxMediaMS + 1}} {
		if validateFrames("/v.mp4", ts) == nil {
			t.Errorf("accepted frame times %v", ts)
		}
	}
	if validateFrames("v.mp4", []int64{0}) == nil {
		t.Error("accepted a relative video path")
	}
}

func TestHelloMediaModalities(t *testing.T) {
	h := helloReply{Protocol: 1, Model: "m", Precision: "p", Dims: 256, MaxBatch: 8,
		Modalities: []string{"text", "image", "pdf", "video", "audio", "smell"}}
	i, err := h.info()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []kind.Kind{kind.Image, kind.PDF, kind.Audio, kind.Video} {
		if !i.Kinds.Has(k) {
			t.Errorf("kinds %b lack %s", i.Kinds, k)
		}
	}
	h.Modalities = []string{"text", "video"}
	if _, err := h.info(); err == nil {
		t.Error("accepted video without image")
	}
}

// writeFakeTalk writes a fake recording: speech in the first and third
// windows, a scene change at 50 s.
func writeFakeTalk(t *testing.T, path string) {
	t.Helper()
	body := "duration 95000\n" +
		"frame 0 a whiteboard with diagrams\n" +
		"frame 50000 a red sports car\n" +
		"audio 1000 welcome to the quarterly review\n" +
		"audio 61000 now the budget\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSidecarAV(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "talk.mp4")
	writeFakeTalk(t, p)
	f := &Fake{Dims: 256}

	s := startFake(t, "FAKE_MODALITIES=text,image,audio,video")
	m, err := s.ProbeMedia(ctx, Index, p)
	if err != nil || m != (MediaInfo{DurationMS: 95000, Audio: true, Video: true}) {
		t.Fatalf("probe: %+v, %v", m, err)
	}
	v, err := s.EmbedAudio(ctx, Index, AudioRequest{Path: p, StartMS: 60000, EndMS: 90000})
	if err != nil || !slices.Equal(v, f.vector("now the budget")) {
		t.Fatalf("audio: %v", err)
	}
	if v, err := s.EmbedAudio(ctx, Query, AudioRequest{Path: p, StartMS: 30000, EndMS: 60000}); err != nil || v != nil {
		t.Fatalf("silent window: %v, %v", v, err)
	}
	vs, err := s.EmbedFrames(ctx, Index, p, []int64{2500, 52500})
	if err != nil || len(vs) != 2 || !slices.Equal(vs[1], f.vector("a red sports car")) {
		t.Fatalf("frames: %v", err)
	}
	// Requests semd should never send are refused before reaching the sidecar.
	if _, err := s.EmbedAudio(ctx, Index, AudioRequest{Path: p, StartMS: 0, EndMS: 31000}); err == nil {
		t.Fatal("oversize audio window accepted")
	}
	// A file the decoder cannot read fails the file, not the daemon.
	junk := filepath.Join(t.TempDir(), "junk.mp4")
	if err := os.WriteFile(junk, []byte("\xff"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProbeMedia(ctx, Index, junk); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("junk probe: %v", err)
	}

	text := startFake(t)
	if _, err := text.ProbeMedia(ctx, Index, p); err == nil || errors.Is(err, ErrFatal) {
		t.Fatalf("probe without the modality: %v", err)
	}
	if _, err := text.EmbedFrames(ctx, Index, p, []int64{0}); err == nil {
		t.Fatal("frames without the modality")
	}
}

func TestSidecarSilentWithVectorIsFatal(t *testing.T) {
	p := filepath.Join(t.TempDir(), "talk.mp4")
	writeFakeTalk(t, p)
	s := startFake(t, "FAKE_MODALITIES=text,image,audio,video", "FAKE_BAD=silentvec")
	_, err := s.EmbedAudio(context.Background(), Index, AudioRequest{Path: p, StartMS: 30000, EndMS: 60000})
	if !errors.Is(err, ErrFatal) {
		t.Fatalf("err %v, want ErrFatal", err)
	}
}

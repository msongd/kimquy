package embed

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/msongd/kimquy/internal/fixture"
	"github.com/msongd/kimquy/internal/kind"
)

// speak renders text with the macOS voice to 16 kHz mono samples.
func speak(t testing.TB, dir, name, text string) []int16 {
	t.Helper()
	p := filepath.Join(dir, name+".wav")
	out, err := exec.Command("/usr/bin/say", "--file-format=WAVE", "--data-format=LEI16@16000", "-o", p, text).CombinedOutput()
	if err != nil {
		t.Skipf("say unavailable: %v %s", err, out)
	}
	s, err := fixture.ReadWAV(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// pad places speech at atMS within a silent recording lasting totalMS.
func pad(speech []int16, atMS, totalMS int) []int16 {
	out := make([]int16, totalMS*fixture.AudioRate/1000)
	copy(out[atMS*fixture.AudioRate/1000:], speech)
	return out
}

func rank(t *testing.T, s *Sidecar, query string, vecs [][]float32) (best int, scores []float64) {
	t.Helper()
	q, err := s.EmbedTexts(context.Background(), Query, []string{QueryPrompt(query)})
	if err != nil {
		t.Fatal(err)
	}
	for j, v := range vecs {
		scores = append(scores, dot(q[0], v))
		if scores[j] > scores[best] {
			best = j
		}
	}
	return best, scores
}

func TestModelAudio(t *testing.T) {
	s := startModel(t)
	if !s.Info().Kinds.Has(kind.Audio) {
		t.Skipf("sidecar offers no audio: %+v", s.Info())
	}
	ctx := context.Background()
	dir := t.TempDir()
	clips := []struct{ name, text, query string }{
		{"meeting", "The project meeting has been moved to Thursday afternoon at three.", "meeting rescheduled"},
		{"recipe", "Mix the flour, sugar and eggs, then bake the cake for forty minutes.", "baking a cake recipe"},
		{"weather", "Heavy rain and strong winds are expected across the coast tonight.", "storm weather forecast"},
		{"football", "He shoots, he scores! The home team wins the cup in the final minute.", "a football match goal"},
	}
	vecs := make([][]float32, len(clips))
	for i, c := range clips {
		p := filepath.Join(dir, c.name+".mov")
		samples := speak(t, dir, c.name, c.text)
		if err := fixture.MOV(p, 64, 64, []fixture.Scene{{MS: len(samples) * 1000 / fixture.AudioRate}}, samples); err != nil {
			t.Fatal(err)
		}
		m, err := s.ProbeMedia(ctx, Index, p)
		if err != nil || !m.Audio || !m.Video {
			t.Fatalf("%s: probe %+v %v", c.name, m, err)
		}
		t0 := time.Now()
		v, err := s.EmbedAudio(ctx, Index, AudioRequest{Path: p, StartMS: 0, EndMS: min(m.DurationMS, MaxAudioWindowMS)})
		if err != nil || v == nil {
			t.Fatalf("%s: %v (silent: %v)", c.name, err, v == nil)
		}
		t.Logf("%s: %d ms of speech embedded in %v", c.name, m.DurationMS, time.Since(t0).Round(time.Millisecond))
		if n := norm(v); math.Abs(n-1) > 1e-3 {
			t.Fatalf("%s: norm %.4f", c.name, n)
		}
		vecs[i] = v
	}
	correct := 0
	for i, c := range clips {
		best, sc := rank(t, s, c.query, vecs)
		t.Logf("%-28q → %-9s %.3f (want %s %.3f)", c.query, clips[best].name, sc[best], c.name, sc[i])
		if best == i {
			correct++
		}
	}
	t.Logf("text→audio top-1: %d/%d", correct, len(clips))
	if correct < len(clips)-1 {
		t.Errorf("text→audio top-1 %d/%d", correct, len(clips))
	}

	// Speech placed 40 s into a minute of silence: only its window carries it.
	long := filepath.Join(dir, "long.mov")
	if err := fixture.MOV(long, 64, 64, []fixture.Scene{{MS: 60_000}}, pad(speak(t, dir, "late", clips[0].text), 40_000, 60_000)); err != nil {
		t.Fatal(err)
	}
	v, err := s.EmbedAudio(ctx, Index, AudioRequest{Path: long, StartMS: 0, EndMS: 30_000})
	if err != nil || v != nil {
		t.Errorf("silent window: vector %v, err %v", v != nil, err)
	}
	v, err = s.EmbedAudio(ctx, Index, AudioRequest{Path: long, StartMS: 30_000, EndMS: 60_000})
	if err != nil || v == nil {
		t.Fatalf("speech window: %v", err)
	}
	if c := dot(v, vecs[0]); c < 0.7 {
		t.Errorf("same speech, offset in a window: cosine %.3f, want ≥ .7", c)
	}

	// A window entirely past the end decodes nothing: silent, not an error.
	if v, err := s.EmbedAudio(ctx, Index, AudioRequest{Path: long, StartMS: 90_000, EndMS: 100_000}); err != nil || v != nil {
		t.Errorf("window past the end: %v %v", v != nil, err)
	}

	junk := filepath.Join(dir, "junk.m4a")
	if err := os.WriteFile(junk, []byte("this is not audio at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProbeMedia(ctx, Index, junk); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("junk probe: %v", err)
	}
	if _, err := s.EmbedAudio(ctx, Index, AudioRequest{Path: junk, StartMS: 0, EndMS: 1000}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("junk audio: %v", err)
	}
	// The sidecar survives bad input.
	if _, err := s.EmbedTexts(ctx, Query, []string{"still alive"}); err != nil {
		t.Fatal(err)
	}
}

func TestModelVideo(t *testing.T) {
	s := startModel(t)
	if !s.Info().Kinds.Has(kind.Video) {
		t.Skipf("sidecar offers no video: %+v", s.Info())
	}
	ctx := context.Background()
	dir := t.TempDir()
	scenes := []struct {
		shapes []fixture.Shape
		query  string
	}{
		{redDisc, "a red circle on a white background"},
		{checker, "a black and white checkerboard pattern"},
		{sunset, "the sun setting over the horizon"},
		{target, "a red and white archery target"},
	}
	var sc []fixture.Scene
	for _, s := range scenes {
		sc = append(sc, fixture.Scene{MS: 10_000, Shapes: s.shapes})
	}
	p := filepath.Join(dir, "slides.mov")
	if err := fixture.MOV(p, 640, 360, sc, nil); err != nil {
		t.Fatal(err)
	}
	m, err := s.ProbeMedia(ctx, Index, p)
	if err != nil || m.Audio || !m.Video || m.DurationMS < 39_000 || m.DurationMS > 41_000 {
		t.Fatalf("probe %+v %v", m, err)
	}
	// One frame from the middle of each scene.
	times := []int64{5000, 15_000, 25_000, 35_000}
	t0 := time.Now()
	vecs, err := s.EmbedFrames(ctx, Index, p, times)
	if err != nil || len(vecs) != len(times) {
		t.Fatalf("frames: %d %v", len(vecs), err)
	}
	t.Logf("%d frames in %v", len(vecs), time.Since(t0).Round(time.Millisecond))
	correct := 0
	for i, s2 := range scenes {
		best, scs := rank(t, s, s2.query, vecs)
		t.Logf("%-42q → frame@%5d %.3f (want @%d %.3f)", s2.query, times[best], scs[best], times[i], scs[i])
		if best == i {
			correct++
		}
	}
	if correct < len(scenes)-1 {
		t.Errorf("text→frame top-1 %d/%d", correct, len(scenes))
	}
	// A frame at the same scene embeds like its neighbour.
	again, err := s.EmbedFrames(ctx, Index, p, []int64{12_000})
	if err != nil {
		t.Fatal(err)
	}
	if c := dot(again[0], vecs[1]); c < 0.95 {
		t.Errorf("two frames of one still scene: cosine %.3f", c)
	}
	if _, err := s.EmbedAudio(ctx, Index, AudioRequest{Path: p, StartMS: 0, EndMS: 1000}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("audio of a silent movie: %v", err)
	}
}

func BenchmarkModelAudioWindow(b *testing.B) {
	s := startModel(b)
	dir := b.TempDir()
	speech := speak(b, dir, "s", "The quarterly numbers look strong, and the team will hire two engineers next month.")
	p := filepath.Join(dir, "b.mov")
	if err := fixture.MOV(p, 64, 64, []fixture.Scene{{MS: 30_000}}, pad(speech, 0, 30_000)); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, err := s.EmbedAudio(context.Background(), Index, AudioRequest{Path: p, StartMS: 0, EndMS: 30_000}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkModelFrames8(b *testing.B) {
	s := startModel(b)
	p := filepath.Join(b.TempDir(), "b.mov")
	if err := fixture.MOV(p, 1280, 720, []fixture.Scene{{MS: 20_000, Shapes: sunset}, {MS: 20_000, Shapes: checker}}, nil); err != nil {
		b.Fatal(err)
	}
	times := []int64{2500, 7500, 12_500, 17_500, 22_500, 27_500, 32_500, 37_500}
	for b.Loop() {
		if _, err := s.EmbedFrames(context.Background(), Index, p, times); err != nil {
			b.Fatal(err)
		}
	}
}

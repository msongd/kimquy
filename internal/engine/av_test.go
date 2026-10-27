package engine

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/msongd/kimquy/internal/embed"
	"github.com/msongd/kimquy/internal/kind"
)

func TestAudioWindows(t *testing.T) {
	for _, c := range []struct {
		dur  int64
		want []span
		cut  bool
	}{
		{50, nil, false}, // under the encoder's minimum
		{400, []span{{0, 400}}, false},
		{30_000, []span{{0, 30_000}}, false},
		{30_500, []span{{0, 30_000}}, false}, // half-second tail dropped
		{31_000, []span{{0, 30_000}, {30_000, 31_000}}, false},
		{maxAudioMS, nil, false},
		{maxAudioMS + 1, nil, true},
	} {
		got, cut := audioWindows(c.dur)
		if c.want == nil && c.dur >= maxAudioMS {
			// Full-length: check the shape rather than spell it out.
			if len(got) != maxAudioMS/embed.MaxAudioWindowMS || got[len(got)-1].end != maxAudioMS || cut != c.cut {
				t.Errorf("%d ms: %d windows ending %d, cut %v", c.dur, len(got), got[len(got)-1].end, cut)
			}
			continue
		}
		if !slices.Equal(got, c.want) || cut != c.cut {
			t.Errorf("%d ms: %v cut %v, want %v cut %v", c.dur, got, cut, c.want, c.cut)
		}
	}
}

func TestFrameSpans(t *testing.T) {
	if got := frameSpans(3000); !slices.Equal(got, []span{{0, 3000}}) {
		t.Errorf("3 s: %v", got)
	}
	if got := frameSpans(12_000); !slices.Equal(got, []span{{0, 4000}, {4000, 8000}, {8000, 12_000}}) {
		t.Errorf("12 s: %v", got)
	}
	for _, d := range []int64{1, 7, 95_000, 3_600_000, embed.MaxMediaMS} {
		got := frameSpans(d)
		if len(got) == 0 || len(got) > maxVideoFrames || got[0].start != 0 || got[len(got)-1].end != d {
			t.Fatalf("%d ms: %d spans %v…", d, len(got), got[:min(3, len(got))])
		}
		for i, s := range got {
			if s.end <= s.start || (i > 0 && s.start != got[i-1].end) {
				t.Fatalf("%d ms: span %d %v not contiguous", d, i, s)
			}
		}
	}
	if n := len(frameSpans(3_600_000)); n != maxVideoFrames {
		t.Errorf("an hour of video: %d frames, want %d", n, maxVideoFrames)
	}
}

// Audio and video are embedded as timed windows and frames. The fake reads
// a text script of what is heard and shown when (see embed.Fake).
func TestAV(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	write(t, filepath.Join(root, "talk.mp4"), "duration 95000\n"+
		"frame 0 a whiteboard with diagrams\n"+
		"frame 50000 a red sports car\n"+
		"audio 1000 welcome to the quarterly review\n"+
		"audio 61000 now the budget\n")
	// Longer than the audio cap: speech past 5:00 is not embedded.
	write(t, filepath.Join(root, "podcast.mp3"), "duration 400000\n"+
		"audio 10000 gardening tips for tomatoes\n"+
		"audio 320000 zebra migration\n")
	write(t, filepath.Join(root, "quiet.wav"), "duration 5000\naudio 0\n")
	write(t, filepath.Join(root, "cover.m4a"), "duration 5000\nframe 0 album art\n") // no audio track
	write(t, filepath.Join(root, "notes.txt"), "Shopping list: milk, eggs and bread for the week.")

	h := start(t, state, root, &embed.Fake{Dims: 768})
	if _, err := h.e.SetRoots([]string{root}, nil); err != nil {
		t.Fatal(err)
	}
	h.idle()
	if st := h.e.Status(); st.Indexed != 4 || st.Failed != 1 {
		t.Fatalf("status %+v, failures %+v", st, h.e.Failures())
	}
	if f := h.e.Failures(); !strings.HasSuffix(f[0].Path, "cover.m4a") || !strings.Contains(f[0].Error, "no audio track") {
		t.Fatalf("failures %+v", f)
	}

	talk := filepath.Join(root, "talk.mp4")
	hits := h.search("red sports car", 1)
	if l := hits[0].Locator; hits[0].Path != talk || hits[0].Kind != "video" || l.Kind != kind.LocTime || l.Start < 50000 {
		t.Fatalf("frame hit %+v", hits[0])
	}
	hits = h.search("the budget", 1)
	if l := hits[0].Locator; hits[0].Path != talk || l != (kind.Locator{Kind: kind.LocTime, Start: 60000, End: 90000}) {
		t.Fatalf("speech hit %+v", hits[0])
	}
	hits = h.search("tomatoes", 1)
	if l := hits[0].Locator; !strings.HasSuffix(hits[0].Path, "podcast.mp3") || hits[0].Kind != "audio" ||
		l != (kind.Locator{Kind: kind.LocTime, Start: 0, End: 30000}) || hits[0].Snippet != "" {
		t.Fatalf("audio hit %+v", hits[0])
	}

	// talk: 19 frames (one per 5 s) + 2 of 4 audio windows (the others silent).
	// podcast: 1 of 10 windows; zebra lies past the cap.
	for name, n := range map[string]uint32{"talk.mp4": 19 + 2, "podcast.mp3": 1, "quiet.wav": 0} {
		f, ok := h.e.cat.Lookup(filepath.Join(root, name))
		if !ok || f.NVec != n {
			t.Errorf("%s: %+v, want %d vectors", name, f, n)
		}
	}
	h.stop()
}

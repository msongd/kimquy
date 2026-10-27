package embed

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Limits of the audio/video ops, shared by semd and semd-embed. semd plans
// the windows and frame times, so each call stays short and a query waits
// behind at most one of them.
const (
	// MaxAudioWindowMS is the longest audio window one embed_audio call
	// embeds: the audio encoder's input limit.
	MaxAudioWindowMS = 30_000
	// MinAudioWindowMS is the shortest window worth embedding.
	MinAudioWindowMS = 100
	// MaxFramesPerCall bounds one embed_frames call.
	MaxFramesPerCall = 8
	// MaxMediaMS is the longest duration a probe may report (a week).
	MaxMediaMS = 7 * 24 * 3600 * 1000
)

// MediaInfo describes an audio or video file.
type MediaInfo struct {
	DurationMS int64
	Audio      bool // has an audio track
	Video      bool // has a video track
}

func (m MediaInfo) check() error {
	switch {
	case m.DurationMS < 1 || m.DurationMS > MaxMediaMS:
		return fmt.Errorf("media duration %d ms out of 1..%d", m.DurationMS, MaxMediaMS)
	case !m.Audio && !m.Video:
		return fmt.Errorf("media has neither audio nor video")
	}
	return nil
}

// AudioRequest asks for one vector for [StartMS, EndMS) of the audio of Path.
type AudioRequest struct {
	Path           string
	StartMS, EndMS int64
}

func (r AudioRequest) validate() error {
	switch {
	case !filepath.IsAbs(r.Path):
		return fmt.Errorf("audio path %q is not absolute", r.Path)
	case r.StartMS < 0 || r.EndMS > MaxMediaMS:
		return fmt.Errorf("audio window %d-%d ms out of range", r.StartMS, r.EndMS)
	case r.EndMS-r.StartMS < MinAudioWindowMS || r.EndMS-r.StartMS > MaxAudioWindowMS:
		return fmt.Errorf("audio window of %d ms, allowed %d..%d", r.EndMS-r.StartMS, MinAudioWindowMS, MaxAudioWindowMS)
	}
	return nil
}

func validateFrames(path string, timesMS []int64) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("video path %q is not absolute", path)
	}
	if len(timesMS) < 1 || len(timesMS) > MaxFramesPerCall {
		return fmt.Errorf("%d frame times, allowed 1..%d", len(timesMS), MaxFramesPerCall)
	}
	for _, t := range timesMS {
		if t < 0 || t > MaxMediaMS {
			return fmt.Errorf("frame time %d ms out of range", t)
		}
	}
	return nil
}

// Fake media files are UTF-8 text standing in for a recording:
//
//	duration 95000
//	audio 0 hello and welcome      (heard from 0 ms on)
//	frame 30000 a red car          (shown from 30000 ms on)
//
// Audio windows embed the words of the audio lines that start inside them
// (none: silent); frames embed the words of the latest frame line.
type fakeMedia struct {
	info   MediaInfo
	audio  []fakeCue
	frames []fakeCue
}

type fakeCue struct {
	ms    int64
	words string
}

func readFakeMedia(path string) (fakeMedia, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return fakeMedia{}, err
	}
	if !utf8.Valid(data) {
		return fakeMedia{}, fmt.Errorf("%w: fake media is not UTF-8", ErrInvalidInput)
	}
	var m fakeMedia
	for i, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		bad := fmt.Errorf("%w: fake media line %d: %q", ErrInvalidInput, i+1, line)
		if len(f) < 2 {
			return fakeMedia{}, bad
		}
		ms, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil || ms < 0 {
			return fakeMedia{}, bad
		}
		cue := fakeCue{ms, strings.Join(f[2:], " ")}
		switch f[0] {
		case "duration":
			m.info.DurationMS = ms
		case "audio":
			m.info.Audio = true
			m.audio = append(m.audio, cue)
		case "frame":
			m.info.Video = true
			m.frames = append(m.frames, cue)
		default:
			return fakeMedia{}, bad
		}
	}
	if err := m.info.check(); err != nil {
		return fakeMedia{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	return m, nil
}

func (f *Fake) ProbeMedia(ctx context.Context, _ Priority, path string) (MediaInfo, error) {
	if err := ctx.Err(); err != nil {
		return MediaInfo{}, err
	}
	m, err := readFakeMedia(path)
	return m.info, err
}

func (f *Fake) EmbedAudio(ctx context.Context, _ Priority, req AudioRequest) ([]float32, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m, err := readFakeMedia(req.Path)
	if err != nil {
		return nil, err
	}
	if !m.info.Audio {
		return nil, fmt.Errorf("%w: no audio track", ErrInvalidInput)
	}
	var words []string
	for _, c := range m.audio {
		if c.ms >= req.StartMS && c.ms < req.EndMS && c.words != "" {
			words = append(words, c.words)
		}
	}
	if len(words) == 0 {
		return nil, nil // silent
	}
	return f.vector(strings.Join(words, " ")), nil
}

func (f *Fake) EmbedFrames(ctx context.Context, _ Priority, path string, timesMS []int64) ([][]float32, error) {
	if err := validateFrames(path, timesMS); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m, err := readFakeMedia(path)
	if err != nil {
		return nil, err
	}
	if !m.info.Video {
		return nil, fmt.Errorf("%w: no video track", ErrInvalidInput)
	}
	out := make([][]float32, len(timesMS))
	for i, t := range timesMS {
		words := ""
		for _, c := range m.frames {
			if c.ms <= t {
				words = c.words
			}
		}
		out[i] = f.vector(words)
	}
	return out, nil
}

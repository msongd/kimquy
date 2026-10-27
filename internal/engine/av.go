package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/msongd/kimquy/internal/embed"
	"github.com/msongd/kimquy/internal/kind"
	"github.com/msongd/kimquy/internal/store"
)

// Caps on what one audio or video file contributes. Files are never
// refused for length; what lies past a cap is skipped and reported.
const (
	// maxAudioMS: audio is embedded in windows over this much of a file.
	maxAudioMS = 5 * 60 * 1000
	// maxVideoFrames: frames sampled per video, spread over its length.
	maxVideoFrames = 32
	// videoFrameEveryMS: shorter videos get one frame per this much time.
	videoFrameEveryMS = 5000
	// minTailMS: a last audio window shorter than this is dropped (unless
	// it is the whole file); a fraction of a second rarely carries meaning.
	minTailMS = 1000
)

// span is [start, end) in milliseconds.
type span struct{ start, end int64 }

func (s span) loc() kind.Locator { return kind.Locator{Kind: kind.LocTime, Start: s.start, End: s.end} }

// audioWindows splits the first maxAudioMS of a recording into windows of
// embed.MaxAudioWindowMS. cut reports audio past the cap.
func audioWindows(durMS int64) (wins []span, cut bool) {
	end := min(durMS, maxAudioMS)
	for s := int64(0); s < end; s += embed.MaxAudioWindowMS {
		w := span{s, min(s+embed.MaxAudioWindowMS, end)}
		if w.end-w.start < max(minTailMS, embed.MinAudioWindowMS) && (s > 0 || w.end-w.start < embed.MinAudioWindowMS) {
			break
		}
		wins = append(wins, w)
	}
	return wins, durMS > maxAudioMS
}

// frameSpans divides a video into equal spans, one frame each, sampled at
// the span's middle.
func frameSpans(durMS int64) []span {
	n := min(maxVideoFrames, max(1, (durMS+videoFrameEveryMS-1)/videoFrameEveryMS))
	spans := make([]span, 0, n)
	for i := range n {
		s := span{durMS * i / n, durMS * (i + 1) / n}
		if s.end > s.start {
			spans = append(spans, s)
		}
	}
	return spans
}

// embedAV embeds an audio or video file: frames of the video track and
// windows of the audio track, each located by its time span.
func (e *Engine) embedAV(ctx context.Context, path string, k kind.Kind) ([]store.Record, string, error) {
	m, err := e.media.ProbeMedia(ctx, embed.Index, path)
	if err != nil {
		return nil, "", err
	}
	if k == kind.Audio && !m.Audio {
		return nil, "", fmt.Errorf("%w: no audio track", embed.ErrInvalidInput)
	}
	var (
		recs  []store.Record
		notes []string
	)
	if k == kind.Video && m.Video {
		spans := frameSpans(m.DurationMS)
		for i := 0; i < len(spans); i += embed.MaxFramesPerCall {
			batch := spans[i:min(len(spans), i+embed.MaxFramesPerCall)]
			times := make([]int64, len(batch))
			for j, s := range batch {
				times[j] = (s.start + s.end) / 2
			}
			vecs, err := e.media.EmbedFrames(ctx, embed.Index, path, times)
			if err != nil {
				return nil, "", err
			}
			if len(vecs) != len(batch) {
				return nil, "", fmt.Errorf("embedder returned %d frame vectors for %d times", len(vecs), len(batch))
			}
			for j, v := range vecs {
				if len(v) != e.info.Dims {
					return nil, "", fmt.Errorf("embedder returned %d dims, announced %d", len(v), e.info.Dims)
				}
				recs = append(recs, store.Record{Vec: v, Kind: k, Loc: batch[j].loc()})
			}
		}
	}
	if m.Audio && !e.info.Kinds.Has(kind.Audio) {
		notes = append(notes, "audio track not embedded (no audio model)")
	} else if m.Audio {
		wins, cut := audioWindows(m.DurationMS)
		silent := 0
		for _, w := range wins {
			v, err := e.media.EmbedAudio(ctx, embed.Index, embed.AudioRequest{Path: path, StartMS: w.start, EndMS: w.end})
			if err != nil {
				return nil, "", err
			}
			if v == nil {
				silent++
				continue
			}
			if len(v) != e.info.Dims {
				return nil, "", fmt.Errorf("embedder returned %d dims, announced %d", len(v), e.info.Dims)
			}
			recs = append(recs, store.Record{Vec: v, Kind: k, Loc: w.loc()})
		}
		// Quiet passages are normal; only a wholly silent track is worth a note.
		if silent > 0 && silent == len(wins) {
			notes = append(notes, "audio track is silent")
		}
		if cut {
			notes = append(notes, fmt.Sprintf("audio after %d:00 not embedded (cap)", maxAudioMS/60000))
		}
	}
	return recs, strings.Join(notes, "; "), nil
}

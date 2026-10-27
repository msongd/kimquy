# M4 results: audio and video segments

Measured 2026-10-26 on the same machine as `M0-RESULTS.md`, adding the
`audio-bf16` weights to `text-q8` and `vision-q8`. Model id
`embeddinggemma-2/eb8a067456a3+v9b18ee382729+aa163c6dc2e47`. The audio suffix
changes the id, so stores built by M3 are reindexed.

## What is embedded

| File | Vectors | Locator |
|---|---|---|
| Audio (`.mp3`, `.m4a`, `.wav`, …) | one per 30 s window over the first 5:00; silent windows skipped | `time:start-end` |
| Video (`.mov`, `.mp4`, …) | ≤32 frames, one per 5 s (spread evenly when longer), 70 soft tokens each; plus its audio as above | frame: the span it stands for; audio: its window |

semd plans the windows and frame times, and each sidecar call covers one
window or ≤8 frames, so a query waits behind at most one of them. Decoding
uses AVFoundation only. Duration is capped, not file size, and the index
notes record what was cut ("audio after 5:00 not embedded (cap)"), a track
that is silent throughout, or audio present without an audio model.

## Exit criterion: timestamped hits

`semd` + `semctl` on a 5-minute 720p talk (30 scenes of 10 s, synthetic
speech throughout) plus a voicemail clip, using real weights:

| Query | Top hit |
|---|---|
| sunset gradient with a yellow sun | talk.mov `time:243750-253125ms` (sunset scene 240–250 s) .795 |
| a blue square | talk.mov `time:281250-290625ms` (blue scene 280–290 s) .778 |
| office move in March | talk.mov `time:210000-240000ms` .768 |
| dentist appointment reminder | voicemail.wav `time:0-4626ms` .747 |

Model tests (`internal/embed/model_av_test.go`):

- Text→speech top-1: 4/4 clips, scoring .745–.782.
- Text→frame top-1: 4/4 scenes, scoring .774–.836.
- Speech 40 s into a minute of silence: the silent window is reported
  silent, and the speech window matches the bare clip.
- Junk input returns `invalid` and the sidecar keeps running.

## CPU, memory, latency

All figures are for the sidecar driven directly at index priority, measured
with `/usr/bin/time -l`. Each run includes a 1.6–1.8 s model load.

| Run | Wall | CPU (user+sys) | Peak RSS |
|---|---|---|---|
| Text model only (load) | 1.74 s | 1.77 s | 534 MiB |
| 32 frames of a 5-min 720p video | 6.62 s | 4.49 s | 796 MiB |
| 10 × 30 s audio windows | 8.47 s | 6.19 s | 1173 MiB |
| Full 5-min video: probe + 32 frames + 10 windows | 12.97 s | 8.55 s | 1375 MiB |

Without the load, a worst-case video takes about 11 s and uses about 0.65 of
one core, on a 10-core machine. Per unit:

| Benchmark (warm) | Time |
|---|---|
| One 30 s audio window: decode, mel, encode | 0.59 s |
| 8 frames from a 720p movie: decode, resize, encode | 1.23 s (154 ms/frame) |
| Audio / vision tower load on first use | 0.15–0.32 s / 0.02–0.07 s |

Tower unload was verified with stdin held open. Both encoders unloaded about
2 minutes after their last use, and audio reloaded on demand in 0.15 s.

Most CPU on the audio side is decoding and the port's Swift mel
spectrogram; the encoder itself runs on the GPU. That is within the plan's
"low CPU" intent: indexing is serialized through one sidecar, and semd is
niced.

## Test fixtures

Video fixtures are generated without ffmpeg. `internal/fixture.MOV` writes a
QuickTime file using the Go standard library only: Photo-JPEG frames and a
16-bit PCM (`sowt`) soundtrack, both of which AVFoundation decodes natively.
Speech comes from `say` as 16 kHz WAV, which `fixture.ReadWAV` reads.

## Not yet done

- **Real recordings.** Synthetic speech and still drawings are easy cases.
  Music, noisy rooms and real footage still need a local, uncommitted set.
- **Codec coverage.** Only Photo-JPEG + PCM has been tested end to end.
  H.264/HEVC/AAC go through the same AVFoundation calls, but no fixture
  covers them, because the standard library has no encoder for them.
- **Rotated video.** The preferred track transform is applied, but no
  fixture covers it.
- **Eval.** The retrieval eval corpus has no audio or video items yet; the
  baseline was regenerated for the new model id and its metrics are
  unchanged.
- **Power.** Still needs `sudo powermetrics --samplers cpu_power,gpu_power -i 1000`
  while `BenchmarkModelAudioWindow` / `BenchmarkModelFrames8` run.

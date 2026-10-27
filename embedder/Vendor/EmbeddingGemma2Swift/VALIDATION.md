# Validation for v0.1.0

Measurements were made on 2026-10-06. These are bounded implementation checks
and author-written synthetic retrieval tests, not a broad quality benchmark.

## Provenance

- Google `google/embeddinggemma-2` revision `914f7f89142e33e77833254d9c9b90c3cef7303b`.
- Original weights SHA256 `197a32965d4b1105faf060417baa899e193fb73cd401f42ec9295234d5553d79`.
- MLX Swift0.32.3, Swift Transformers1.3.0, PyTorch2.14.1, Transformers5.19.0.
- Reference scripts use upstream CPU FP32. Packed-kernel validation independently
  unpacks affine UInt32 weights with NumPy before upstream CPU FP32 execution.

## Results

- MacBook Air M3/16 GiB and physical iPhone16ProMax/iOS27.0 validated.
- FP32/BF16 text: seven multilingual/code inputs and one 959-token input;
  independent tokenizer/mask checks, MRL128/256/512/768.
- Images: four fixtures with token budgets70/140/280, aspect ratio and resize
  checks; EXIF1–8 and grayscale. PNG pixels/positions matched the reference.
- Audio: four silence/tone/noise/chirp signals; frontend mask/count checks;
  ten frontend length boundaries through30seconds. Full30second inference
  was not validated. Four short clips passed vector thresholds.
- Original frozen vector criteria: FP32 cosine>=.99999/error<=.0001;
  BF16 cosine>=.999/error<=.01; unit-norm error<=1e-5, finite values, token IDs.
- Both 4/8-bit packed FP32 kernel checks passed (max error<2.38e-7) against
  independently dequantized upstream references on text and an image fixture.
- Selected q8 also passed original BF16-activation vector tolerances on text,
  image and audio fixtures. The audio tower itself is not quantized.
- 15 text regression checks, 11 Swift unit tests, four malformed quantization
  metadata cases passed. Test code and independent audit scripts are included.

## Quantization selection

The recipe was fixed before measurements: affine group64, compatible2D weight
matrices, BF16 activations, remaining norms/scalars/3Dvision table unchanged.
The dev gate required minimum cosine>=.995 to BF16, at most one lost correct
text top1, and preserved synthetic shape top1. Choose the smallest passing file.

Both q8 and q4 scored24/24 on the simple bilingual dev. q8 min cosine .9997186
passed; q4 .9739568 failed. The choice was recorded before holdout evaluation.
A separate holdout retained16/16 text answers and6/6 text→image matches with
q8, plus3/3 image→text matches separately for English and Russian. This is
not evidence that all real-world searches will be correct.

Original text+vision weights total877599648bytes; selected q8 totals481214056.
Tokenizer/config/runtime/index storage are additional. q4 is experimental and
is not distributed as the selected profile, but the converter supports it.

## Observed performance

With CLI reusable GPU cache64MiB, a24-item held-out text batch on Mac had warm
median .22436sBF16/.22670sq8 (three repeats after one warmup). Sampled process
peaks were1019364840B/782681408B for text and1560872856B/1142408824B for three
images. A speedup is not established. Do not compare forward-only benchmarks
with these end-to-end encode measurements.

Two q8 iPhone launches passed. Repeat short text .16035s, long .43201s,
images .22189–.98099s, audio .04867–.14289s. Text/image vectors matched Macq8;
one audio fixture differed by up to .001902 within the frozen tolerance.
MLX allocator peak1000478592B includes sequential text, vision and audio;
it is not total process footprint. Loading each tower is measured separately
in the machine-readable summaries.

A Mac BF16 image attempt was stopped by the +256MiB system-swap watchdog;
reusable cache was then limited to64MiB and subsequent holdout runs passed
with no positive swap growth. This is recorded, not omitted from evidence.

## Known limitations

- Real collection quality, battery, thermals, long-context stress and older
  devices/OS versions remain unmeasured; package OS minima are declarations.
- Audio Russian-description smoke achieved only1/4 matches in each direction.
  Audio numerical correctness does not establish useful audio retrieval quality.
- No video, interleaved multimodal documents, generation, ASR or TTS.
- Transparent images are rejected. Exact JPEG/HEIC/color-profile decoder parity
  is not established. Audio requires mono16kHz PCM16 WAV or normalized Float samples.
- This package's quantized format is not a drop-in Python mlx-vlm checkpoint.

The `Validation/` directory contains aggregate audit results without weights,
raw embedding vectors, local paths, device identifiers or signing material.

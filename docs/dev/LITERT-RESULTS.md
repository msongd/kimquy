# LiteRT-LM results: the embedder off MLX

Built 2026-11-05 on the same machine as `M0-RESULTS.md`. `semd-embed` now
runs EmbeddingGemma 2 through the LiteRT-LM v0.19.0 C API instead of MLX
Swift and the vendored EmbeddingGemma2Swift port.

## What changed

| | Before (MLX) | Now (LiteRT-LM) |
|---|---|---|
| Model | Nurymanau/EmbeddingGemma-2-MLX-Swift: text-q8, vision-q8, audio-bf16, three parts | litert-community/embeddinggemma-2-740m-litert-lm: one int4 `.litertlm` file, 485 MB, pinned by revision and SHA-256 |
| Download | 320 MB text, plus 193 MB vision and 611 MB audio if chosen | 485 MB, everything |
| Runtime | MLX Swift built from source (xcodebuild, a metallib, three resource bundles) | `libCLiteRTLM_mac.dylib` from the release, pinned by checksum, in Contents/Frameworks |
| Build | `xcodebuild` | `swift build -c release --arch arm64` |
| Slim app | 45 MB | 90 MB (the dylib, thinned to arm64, is 64 MB) |
| Bundled app | 1.1 GB | 552 MB |
| Model code we own | the vendored port (towers, processors, quantization) | `Engine.swift`: about 180 lines over the C API |

The model ID becomes `embeddinggemma-2-litert/e7a8a2204b91`, so semd sees a
new model and rebuilds the index. After the new model is installed, the old
part folders (`text-q8`, `vision-q8`, `audio-bf16`) are deleted.

## Measurements

| Check | MLX | LiteRT-LM |
|---|---|---|
| Retrieval eval (`internal/engine`), all 84 queries | top1 0.952, recall@5 1.000, MRR@10 0.968 | top1 0.952, recall@5 1.000, MRR@10 0.966 |
| Text indexing, semd CPU time (same tree) | 164.7 s | 10.6 s |
| Text throughput | 7.3 chunks/s | 11.7 chunks/s |
| Screenshots (214 queries), hit@1 / hit@5 | 143 / 179 | 126 / 174 |
| Image embed p50 | 1020 ms | 434 ms |
| RSS, text and images | 1004 MB | 692 MB |
| Audio, per 30 s window | 0.82 s | 1.86 s |
| RSS, audio and video | 1393 MB | 593 MB |
| Cold start | | 6.1 s, then 0.6 s with the GPU program cache |

The retrieval eval per group (text, PDF, image, non-English) matches the MLX
baseline; `baseline.json` was rewritten for the new model ID. Screenshots,
which need text read from pixels, lose 17 hits at 1 but only 5 at 5. Audio
is slower but still far faster than real time.

## The GPU program cache

LiteRT-LM compiles GPU programs at load, about 6 s. With a cache folder it
keeps them, about 310 MB, and later starts take 0.6 s. semd passes
`--cache <state>/cache/litert`. semd-embed:

- writes a `MODEL` stamp there (the model's hash, size, inode and mtime);
- on a mismatch deletes only the entries named after the model file, logs
  `cache … cleared`, and writes the new stamp;
- marks the folder excluded from Time Machine.

## Packaging

- The dylib ships universal and unsigned. `build.sh` thins it to arm64,
  puts it in Contents/Frameworks and signs it before semd-embed.
- semd-embed keeps two rpaths only: `/usr/lib/swift` and
  `@executable_path/../Frameworks`. `build.sh` deletes the build's
  `@loader_path` and the toolchain folders of the build Mac, and fails if
  any other rpath remains.
- The hardened runtime loads only libraries from the signer's team. An ad
  hoc signature has no team, so an ad hoc semd-embed is signed without the
  hardened runtime. A Developer ID signs both with one team, and the
  release keeps the hardened runtime.
- `BUNDLE_MODELS=1` checks that MODELS holds only `litert/<model file>` and
  that its SHA-256 is the one pinned in `internal/model`.

```sh
make embed     # → embedder/.build/out/Products/Release/{semd-embed,libCLiteRTLM_mac.dylib}
SEMD_EMBED_BIN=embedder/.build/out/Products/Release/semd-embed SEMD_MODEL=models/dev \
	go test ./internal/embed ./internal/engine -run 'Model|Eval'
```

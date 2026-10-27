# M0 spike results

Measured 2026-10-26 on a MacBook Pro M1 Pro (MacBookPro18,1), 32 GB, macOS 27.0.1,
Xcode 27.1 (Swift 6.4), mlx-swift 0.32.3, weights `text-q8` (affine8-g64), output
256 dims. Reproduce with the commands in `internal/embed/model_test.go`.

| Check | Result |
|---|---|
| Build (Release, no coverage instrumentation) | OK, 16 MB binary + `mlx-swift_Cmlx.bundle` (2.3 MB metallib) |
| Retrieval parity with the port's q8 results (dev / holdout) | 24/24, 16/16 top-1 (port: 24/24, 16/16 at 768 dims); smallest margin .057 |
| Batch invariance (alone vs padded batch) | cosine ≥ .999 |
| Repeat determinism | cosine ≥ .99999 |
| Cold start: spawn + load + hello | 1.66–1.77 s |
| Query latency, warm (1 short query) | 22 ms |
| Index throughput, warm (batch of 32 × ~1500-char chunks) | 1.69 s/batch, ~19 chunks/s |
| Peak RSS of semd-embed during benchmarks | 567 MiB |

Not yet done:

- Golden vectors: the port publishes only aggregate audit results, no raw
  vectors, so parity is checked by retrieval above instead.
- Power: needs `sudo powermetrics --samplers cpu_power,gpu_power -i 1000`
  while `BenchmarkModelIndexBatch32` runs.
- Images and PDFs landed in M3 (`M3-RESULTS.md`); audio and video in M4
  (`M4-RESULTS.md`).

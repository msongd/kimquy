# M3 results: images, PDFs, retrieval eval

Measured 2026-10-26, same machine and weights as `M0-RESULTS.md`, plus
`vision-q8`. Model id `embeddinggemma-2/eb8a067456a3+v9b18ee382729` (text and
vision manifests).

## Retrieval eval (exit criterion)

`internal/engine/testdata/eval/corpus.json`: 42 generated documents and 91
queries, indexed and searched through the whole pipeline (crawl → sidecar →
store → search). The documents are 26 text and code files, 4 PDFs with text
pages and vector-drawn figure pages, and 12 drawn images with neutral names.
The queries are 84 answerable ones plus 7 that nothing in the corpus answers.
A hit counts when the path matches, and for PDFs when the page matches too.
Baseline: `testdata/eval/baseline.json`.

| Group | n | top-1 | recall@5 | MRR@10 |
|---|---|---|---|---|
| all | 84 | .952 | 1.000 | .968 |
| text/code | 44 | .932 | 1.000 | .956 |
| pdf (page-exact) | 20 | .950 | 1.000 | .960 |
| image | 20 | 1.000 | 1.000 | 1.000 |
| non-English (vi, ru, fr) | 6 | 1.000 | 1.000 | 1.000 |

The 4 misses are indirect paraphrases where a near-topic document wins
("how many people are we hiring next quarter" → annual report page 1).

Top-1 scores, which feed the planned min-score threshold:

| Case | min | median | max |
|---|---|---|---|
| top-1 is relevant (80) | .671 | .762 | .860 |
| top-1 is wrong (4) | .644 | .689 | .691 |
| nothing answers (7) | .607 | .645 | .711 |

The ranges overlap. A cut near .65 would hide 4 of the 7 unanswerable
queries without losing a correct hit. "Italian flag" scores .711 against the
French tricolour, so a hard cut is risky; better to dim low scores in the
UI than to drop them.

Run: `SEMD_EMBED_BIN=… SEMD_MODEL=models/release go test ./internal/engine -run Eval -v`.
A second run against the baseline passes; `SEMD_EVAL_UPDATE=1` rewrites it.

## Media performance

| Check | Result |
|---|---|
| Image, 12 MP PNG: decode, resize, embed | 0.72 s each, warm |
| Vision weights: lazy load / unload after 120 s idle | 0.02–0.07 s / verified; vector identical after reload |
| Peak RSS of semd-embed with vision loaded | 913 MiB (text only: 567 MiB) |
| Eval corpus indexing: 42 docs including 12 images and 4 PDFs | 11.3 s |
| Text→image top-1 on 5 drawings (`TestModelImage`) | 5/5 |
| Rendered PDF figure page vs PNG of the same drawing | cosine .973 |
| Transparent PNG vs same drawing opaque | ≥ .9 (flattened on white) |

## Not yet done

- Real photos: the eval's images are synthetic drawings, so the image
  scores are optimistic. A local, uncommitted photo set would show more.
- Scanned PDFs with text in the image: figure pages are shapes only.
- Speed: most of the image time is the port's pure-Swift Pillow-exact
  resize; replacing it with vImage would trade exactness for speed and needs
  an eval rerun.
- Untested paths: EXIF-rotated JPEGs and password-locked PDFs. The code
  handles both (ImageIO transform; locked → `invalid`), but no fixture
  covers them yet.
- Power: still needs `sudo powermetrics`.

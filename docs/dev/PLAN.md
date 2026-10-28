# Local Semantic File Search — Implementation Plan

Offline, multimodal semantic search over user-chosen directories on macOS, powered by
Google **EmbeddingGemma 2** (`google/embeddinggemma-2`, released 2026-10-06).

## 1. Decisions (locked)

| Topic | Decision |
|---|---|
| Platform | **macOS 14+, Apple Silicon only** (MLX requirement). Linux / Intel dropped. |
| Model | EmbeddingGemma 2: text 270M + vision 170M + audio 300M, shared 768-d space, 8K ctx |
| Inference | **Swift sidecar `semd-embed`** on **MLX Swift** (Metal GPU, so low CPU) |
| Model code | **Vendor** [EmbeddingGemma2Swift](https://github.com/Obscyra-app/EmbeddingGemma2Swift) (MIT), pinned, and guarded by golden-vector tests |
| Precision | **8-bit** text/vision + **bf16** audio (the port's validated profile). q4 is opt-in only if it passes our retrieval eval. Changing precision forces a full reindex. |
| Media decoding | macOS frameworks only: **ImageIO** (all image formats incl. HEIC/WebP), **AVFoundation** (audio/video), **PDFKit** (PDF text + page render). No ffmpeg. |
| PDF | Text per page. Pages with little or no text are rendered to an image and embedded with the vision encoder. |
| Video | **Per-segment vectors**: ≤32 uniformly sampled keyframes at the 70-token vision budget (each with a timestamp), plus audio in 30 s segments for the first 5 min |
| Search | **Semantic only** (no keyword/BM25 index) |
| Change detection | **FSEvents** via cgo (system framework, no third-party package) |
| Daemon | Go, stdlib-first |
| UI | MyGo (native GPU UI, no webview), a thin API client |
| Vector DB | Built in, pure Go, no external DB |

## 2. Architecture

```
SemSearch.app
├── Contents/MacOS/SemSearch          MyGo UI (pure Go, no cgo)
└── Contents/Helpers/
    ├── semd                          core daemon (Go + small cgo for FSEvents)
    └── semd-embed                    Swift: MLX model + ImageIO/AVFoundation/PDFKit

 UI ──HTTP/JSON over Unix socket──▶ semd ──NDJSON over stdin/stdout──▶ semd-embed
     ~/Library/Application Support/SemSearch/semd.sock (0600)
```

- **semd** owns all state: config, file catalog, vector store, scheduler, API.
- **semd-embed** is stateless. It turns *paths and strings* into *vectors and extracted text*.
  semd spawns and supervises it, restarting it with backoff. Crash loops surface in `/status`; they are never hidden.
- The UI holds no state. Everything goes through the API. The `semctl` CLI uses the same API (for dev, tests and scripting).
- Nothing listens on TCP, and there is no network code at all. Offline holds by construction.

## 3. Repo layout

```
cmd/semd/              daemon main
cmd/semctl/            CLI client
internal/api/          net/http handlers, request validation
internal/config/       roots, excludes, limits (JSON file, validated at load)
internal/catalog/      file records: path, size, mtime, sha256, status, error
internal/watch/        FSEvents (fsevents_darwin.go + cgo shim), event-ID persistence
internal/crawl/        initial and recovery scans
internal/extract/      plain text (UTF-8 sniff), Office (archive/zip + encoding/xml)
internal/chunk/        text chunker (token-aware, paragraph/line boundaries)
internal/embed/        sidecar client: protocol, supervision, backpressure
internal/sched/        priority queue, CPU/thermal/battery policy
internal/store/        vector store (segments, tombstones, compaction, search)
embedder/              Swift package: semd-embed + vendored EG2 port (+ LICENSE notices)
ui/                    MyGo app
packaging/             app bundle, LaunchAgent plist, signing/notarization scripts
testdata/              fixtures + golden vectors
```

## 4. Core daemon (`semd`)

### 4.1 API (lean, versioned under `/v1`)

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/search` | `{q, k≤100, roots?, kinds?}` → hits `{path, kind, locator, score, snippet}` |
| GET/PUT | `/v1/roots` | List / replace indexed directories and excludes |
| GET | `/v1/status` | Counts (indexed/pending/failed), embedder state, paused reason |
| GET | `/v1/events` | SSE stream of progress and errors (for the UI) |
| POST | `/v1/pause`, `/v1/resume` | Manual indexing control |
| POST | `/v1/reindex` | Full rebuild (also required after a precision change) |
| GET | `/v1/failures` | Files that failed, with reasons (fail loud, visibly) |
| POST | `/v1/preview` | `{path, locator?}` → the text around the match and the file's details (quick view) |
| GET | `/v1/thumbnail?path=` | An indexed image, small enough to show (PNG/JPEG/GIF/WebP/BMP as is, else scaled by `sips`) |

`locator` = byte range (text), page number (PDF), or timestamp (audio/video).

### 4.2 Pipeline

1. **Discover**: FSEvents stream over all roots (`kFSEventStreamCreateFlagFileEvents`). The last
   event ID is persisted, so after a restart we replay only the changes since then. On
   `MustScanSubDirs`, `UserDropped` or `KernelDropped`, rescan that subtree.
2. **Filter**: excludes (hidden dirs, `node_modules`, `.git`, user globs), size caps per kind,
   symlink loops, unreadable files (recorded as failures).
3. **Change check**: size+mtime fast path, then a sha256 content hash. An unchanged hash means no work.
4. **Route by kind**:
   - text/code/markdown → Go extract → chunk (≈256 tokens with overlap) → `embed_text` (document prompt
     `title: {filename} | text: {chunk}`)
   - docx/xlsx/pptx → Go extract (zip+XML) → same as text
   - pdf → `extract_pdf` in the sidecar → text chunks per page, plus image vectors for text-less pages
   - image → `embed_image` (280-token budget)
   - audio → `embed_audio` in 30 s segments, first 5 min
   - video → `embed_video`: ≤32 keyframes (70-token budget) + audio segments as above
5. **Commit**: an atomic file-level replace (tombstone old vectors, append new ones, then update the catalog).
6. **Query**: `task: search result | query: {q}` → `embed_text` at query priority → store search →
   snippets read live from disk at the locator. If the file changed since indexing, the hit is marked stale.

### 4.3 Vector store (pure Go)

- Vectors are **truncated to 256-d (Matryoshka), re-normalized, int8-quantized** with a per-vector scale. That's
  ~260 B/vector, so 1M vectors ≈ 260 MB. The dimension and precision profile are written to the header, and a
  mismatch with the running model refuses to load. It fails loudly and asks for a reindex.
- Layout: append-only **segments** (`vectors.bin` contiguous int8 block + `meta.bin` records:
  chunkID, fileID, kind, locator), a CRC32 per record, fsync on segment seal.
- Deletes are **tombstones** (a bitmap per segment). Background **compaction** runs when dead space exceeds 30%.
- Catalog: a write-ahead log + periodic snapshot (`encoding/binary`, `crypto/sha256`). On a
  checksum failure, recovery truncates to the last valid record and logs it.
- Search: brute-force int8 dot product, parallel over segments, top-k heap. The target is under 50 ms at 1M
  vectors on M1. HNSW is deferred until benchmarks prove it's needed.

### 4.4 CPU policy ("just enough")

- The heavy work runs on the GPU (MLX). Go-side work (hashing, extraction) runs on **1 worker by default**
  (configurable), with the process niced.
- The sidecar runs index requests at **utility/background QoS** and queries at **userInitiated**. A pending query
  preempts the index queue.
- Indexing auto-pauses on **battery** (configurable), **Low Power Mode** and **thermal state ≥ serious**. The
  sidecar reports these via `ProcessInfo`.
- The sidecar unloads the audio/vision encoders after 2 min idle and exits after 10 min idle, so the idle footprint is near zero.
  The first query after that has a ~1–2 s cold start (to be measured in M0).
- Bounded queues everywhere. Backpressure stops the crawler, so memory can't grow without limit.

## 5. Embedder sidecar (`semd-embed`, Swift)

- Protocol: **NDJSON over stdin/stdout**, one request per line with an `id`. Vectors are returned as base64
  float32. Stderr carries logs. Ops: `hello` (model id, precision, dims), `embed_text`,
  `embed_image`, `embed_audio`, `embed_video`, `extract_pdf`, `status` (thermal/power).
- The vendored port supplies text/image/audio encoders. We add: video frame sampling
  (AVAssetImageGenerator), audio resampling to 16 kHz mono (AVAudioConverter), PDF page text and render
  (PDFKit), and the idle-unload logic.
- Every input is validated. Oversize inputs are rejected with explicit errors, never silently truncated
  (except the documented caps on video/audio).
- Golden tests: fixed fixtures → vectors compared with the official PyTorch FP32 reference
  (cosine ≥ 0.99 for 8-bit). They run in CI on an Apple Silicon runner.

## 6. UI (MyGo)

- One window: search field (debounced), result list (icon by kind, path, snippet/page/timestamp,
  score), Enter opens the file, ⌘-Enter reveals it in Finder. Video/audio hits open at their timestamp where possible.
- Settings sheet: roots add/remove (folder dialog), excludes, pause-on-battery toggle, status
  and failures list.
- Optional: menu-bar icon and a global shortcut to summon the search window.
- Quick view (M8): an eye on each result, or ⌘I, opens a side pane with the chosen hit's text
  scrolled to the match (or the picture of an image), and the file's details below.

## 7. Packaging and OS integration

- One `.app` bundle. The daemon is registered as a **LaunchAgent** (`~/Library/LaunchAgents`, `launchctl bootstrap`).
  A single-instance lock file stops two daemons from running.
- **Model weights are bundled** in the app/dmg, so search works fully offline from install. They're about 1 GB, to be confirmed in M0.
- **TCC / privacy**: indexing `~/Documents`, `~/Desktop`, `~/Downloads`, iCloud Drive and external volumes needs
  user consent. The UI guides the user through granting access, and the daemon reports `EPERM` roots loudly in `/status`.
- Code signing + notarization (the MLX `metallib` must be included and signed).

## 8. Milestones

| # | Deliverable | Exit criteria |
|---|---|---|
| M0 | **Spike**: vendored port builds; text/image/audio embed on M1 | Golden vectors pass; latency, RAM and power numbers recorded; cold-start time measured |
| M1 | Store + catalog | Unit + fuzz tests; crash-recovery test (kill -9 mid-write); 1M-vector benchmark |
| M2 | Daemon pipeline for text/code/Office + API + `semctl` (fake embedder for tests) | End-to-end index & search on a sample tree |
| M3 | Sidecar integration: real embeddings, PDF, images | Retrieval eval set (≥50 queries) has a baseline score |
| M4 | Audio + video segments | Timestamped hits; CPU/power within budget |
| M5 | FSEvents watcher + event-ID replay + CPU/thermal/battery policy | Edits show up in results within 5 s; no full rescan after a restart |
| M6 | MyGo UI | Search, open/reveal, roots, status |
| M7 | Packaging: LaunchAgent, TCC flow, signing/notarization, dmg | Clean install on a fresh Mac, works with networking disabled |
| M8 | Quick view pane: `/v1/preview`, `/v1/thumbnail`, `semctl preview`, side pane in the UI | A hit previews at its match, with its details, for every kind; the pane follows the chosen row and fits the smallest window |

Note: Go packages for store, catalog, chunker, extractors and the API can be built and tested on any OS
using the fake embedder. The sidecar, FSEvents and packaging need a Mac.

## 9. Risks

| Risk | Mitigation |
|---|---|
| The vendored port is early (v0.1.0, single commit) | Pin it, own a fork, golden tests; gemma-4-swift-mlx PR #66 is an alternative reference |
| 4-bit drift (failed the port's gate) | 8-bit default; q4 only after our own eval |
| Video/audio indexing cost on large libraries | Hard caps, background QoS, pause on battery/thermal |
| MyGo is pre-1.0 (v0.6.x) | The UI is thin and API-only, so it can be swapped for SwiftUI cheaply |
| TCC denials look like "missing results" | Explicit per-root permission status in UI and `/status` |
| Semantic-only search misses exact identifiers | Accepted for v1; the store layout leaves room for a lexical index later |

## 10. Still open (defaults assumed, tell me if wrong)

- Size caps: text 10 MB, PDF 200 MB, image 50 MB, audio/video unlimited size but capped duration as above.
- iWork files (Pages/Keynote/Numbers): v1 embeds their embedded preview image only.
- No OCR in v1. Scanned pages rely on the vision embedding.
- Matryoshka 256-d. Switching to 512/768 means a reindex.

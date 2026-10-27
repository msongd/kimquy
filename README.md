<p align="center"><img src="docs/icon.png" width="128" alt="Kim Quy icon"></p>

# Kim Quy

Local semantic search for your files on macOS. Search by meaning across
text, documents, images, PDFs, audio and video. Everything runs on your
Mac: no account, no cloud, no network after the model download.

It is named after Kim Quy, the Golden Turtle of Vietnamese legend, who keeps
what lies at the bottom of the lake.

## Features

- **Search by meaning.** "how much is the rent" finds the right page of
  `lease.pdf`, even when no words match.
- **Many kinds of file.**
  - Plain text, code and Office documents.
  - Images, including HEIC and WebP.
  - PDFs: text per page, with pages that are scanned images embedded as
    images.
  - Audio, and video as both keyframes and sound.
- **Kept up to date.** FSEvents tracks changes, and indexing pauses on
  battery and under load.
- **Native and light.**
  - A menu-bar app with a global shortcut.
  - The model runs on the GPU through MLX.
  - The daemon is a single Go binary with its own vector store.
- **A small download.** The app is about 45 MB. At first launch it
  downloads the model parts you choose, or imports files you already have.

## Requirements

- macOS 14 or later on Apple Silicon.
- About 320 MB of disk space for the text model.
  - Images, PDFs and video frames add 193 MB.
  - Audio adds 611 MB.

## How it works

```
Kim Quy.app
├── Contents/MacOS/KimQuy        the UI: MyGo, pure Go, no webview
└── Contents/Helpers/
    ├── semd                     the daemon: crawls, watches, indexes, serves a local API
    ├── semd-embed               the embedder: Swift + MLX, runs EmbeddingGemma 2
    └── semctl                   the command line client
```

`semd` runs as a LaunchAgent. It talks to the UI and to `semctl` over a
Unix socket in `~/Library/Application Support/Kim Quy`, which also holds
the index and the model.

## Build

You need Go 1.27 or later and Xcode 16 or later (for `semd-embed`).

```sh
make build   # bin/semd, bin/semctl, bin/KimQuy and semd-embed
make test    # go vet and go test -race in both Go modules
make app     # packaging/out/Kim Quy.app (ad hoc signed)
make dmg     # the app and packaging/out/KimQuy-<version>.dmg
make help    # every target and variable
```

To sign and notarize a release, set
`SIGN_ID="Developer ID Application: … (TEAMID)"` and `NOTARY_PROFILE=…`.
See `packaging/build.sh` for the other options.

## Command line

```sh
semctl search -k 5 "invoice from the plumber"
semctl roots set -exclude 'node_modules' ~/Documents ~/Projects
semctl status
semctl events        # stream indexing progress
```

Run `semctl -h` for all commands.

### When indexing seems stuck

- `semctl status`: `paused` gives the reason (manual, on battery, Low Power
  Mode, hot). `working` lists the files in flight, with `elapsed_s` and
  `done`/`total` vectors (`total` is 0 while unknown). `pending` counts the
  queue, which holds at most 1024 paths, so a large number there is normal.
- `semctl events`: an `indexed` line per file, with its vector count and time.
  A file still running after 5 minutes gets a `slow` line.
- `semctl failures`: files that could not be indexed and why. They are
  retried only when they change.
- The log, `~/Library/Logs/Kim Quy/dev.kimquy.semd.log`, has the same
  `indexed`, `slow file`, retry and pause lines. If it shows no new lines and
  `working` is empty, the Mac may have been asleep:
  `pmset -g log | grep -E " (Sleep|Wake) "`.

## Privacy

Kim Quy reads only the folders you choose. Indexing `~/Documents`,
`~/Desktop`, `~/Downloads` and similar folders needs Full Disk Access for
`semd`; the app explains how to give it. Nothing leaves your Mac. The only
network request it makes is the model download from Hugging Face, pinned to
one revision and checked against its SHA-256 hashes.

To uninstall, use Settings › Service › Uninstall Kim Quy…. It removes the
index, the model, the settings, the logs and the app.

## Layout

| Path | What |
|---|---|
| `cmd/semd`, `cmd/semctl` | the daemon and the CLI |
| `internal/` | crawl, extract, chunk, embed, store, watch, scheduling, API, model download |
| `ui/` | the MyGo app; a separate Go module, so the daemon has no third-party dependencies |
| `embedder/` | `semd-embed`, the Swift sidecar, with the vendored EmbeddingGemma2Swift |
| `packaging/` | the app and dmg build, the icon drawn in Go, and `uninstall.sh` |
| `docs/dev/` | the design plan and the results of each milestone |

## Credits and licenses

Kim Quy is released under the [MIT License](LICENSE).

- [EmbeddingGemma 2](https://huggingface.co/google) by Google. The app
  uses the MLX weights from
  [Nurymanau/EmbeddingGemma-2-MLX-Swift](https://huggingface.co/Nurymanau/EmbeddingGemma-2-MLX-Swift)
  (Apache-2.0), which it downloads; they are not in this repository.
- [EmbeddingGemma2Swift](https://github.com/Obscyra-app/EmbeddingGemma2Swift)
  (MIT), vendored in `embedder/Vendor` with its license and notices.
- [MLX Swift](https://github.com/ml-explore/mlx-swift) (MIT) and
  [MyGo](https://mygo.egoist.dev/).

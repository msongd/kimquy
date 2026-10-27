# M6 results: MyGo UI

Built 2026-10-26 on the same machine as `M0-RESULTS.md`.

## Exit criterion

| Check | Result |
|---|---|
| Search | Debounced (200 ms), results ranked by score; checked against a real semd with the real model |
| Open / reveal | Enter or a double click opens the file; ⌘-Enter shows it in Finder |
| Roots | Add (folder dialog, several at once) and remove; root errors shown, with a Full Disk Access hint for EPERM |
| Status | Footer line, polled every 2 s (event-driven since M7); error banner when semd is unreachable; details in Settings |

Settings also has exclusions, the pause-on-battery switch, Pause/Resume,
Rebuild Index (confirmed first) and the failures list (first 200).

```sh
cd ui && go run .            # semd must be running; -socket to point elsewhere
cd ui && go test ./...       # UI tests: fake semd, MyGo's Tester
```

## Layout

- `ui/` is a **separate Go module** (`semsearch/ui`, `replace semsearch => ../`).
  MyGo and its dependencies (purego, go-text, x/image, x/net, x/text) stay
  out of semd's `go.mod`, which still has no requirements.
- `internal/client` (new) is the API client and wire types. It was moved out
  of `internal/api`, so the UI links only `client`, `config`, `events`,
  `kind` and `fsutil`: no engine and no cgo. The app builds with
  `CGO_ENABLED=0` (16.7 MB binary; about 117 MB RSS when idle).
- `TestClientMirrorsEngine` keeps the client's types in step with the engine's. It
  marshals fully populated engine values and decodes them strictly into the
  client's types, then compares both ways.
- New API: `GET`/`PUT /v1/settings` (`pause_on_battery`), saved to
  `config.json`. A mutex now serializes config writers (`SetRoots`, `SetPauseOnBattery`).

## Design

- The UI state is changed only on the main thread. Calls to semd run in
  goroutines and post their results back with `Window.Update`. Each search
  carries a sequence number, so a late reply to an older query is dropped.
- The app depends on two interfaces, `backend` (semd) and `shell` (open,
  reveal, folder picker). Tests drive the real view with `ui.Tester`, a
  fake semd and a queue for async work. They use no wall-clock waits: the
  debounce and polling run on the Tester's clock.
- The real-model check is `TestRealSemd`. It needs `SEMSEARCH_SOCKET` and
  `SEMSEARCH_QUERY` (otherwise it skips) and renders the window to PNG
  with `SEMSEARCH_SHOT`. It ran against semd over 5 fixture files (3 notes,
  a synthetic voicemail and talk). "when is my dentist appointment" ranked the voicemail (0.73)
  and `dentist.txt` (0.70) first. "soup recipe" ranked `soup.txt` first (0.74).

Bug found while testing: while a status request was out for more than the
poll interval, the view asked for a frame every millisecond: 58,001 frames
in a minute. It now draws nothing until the reply arrives
(`TestIdleWhileStatusPending`).

## Not done

- **Opening audio/video at the hit's time.** `Shell.OpenPath` cannot pass a
  position. The row shows the span (e.g. `4:00–4:30`). QuickTime could be
  driven through `osascript` (`set current time of document 1`). That would
  bypass the user's default player and trigger an Automation permission
  prompt, so it is left for a decision.
- **Menu-bar icon and global shortcut.** Done after M7: see "Menu bar" in
  `M7-RESULTS.md`.
- **Packaging.** Done in M7 (`packaging/build.sh`, see `M7-RESULTS.md`).
- **Live updates.** Done after M7: see "Live status" in `M7-RESULTS.md`.
- **Interactive use.** The window was only launched briefly (it starts
  cleanly with no output). Clicks, dialogs and Finder reveal were exercised
  through the Tester and fakes, not by hand.

# M7 results: Packaging

Built 2026-10-26 on the same machine as `M0-RESULTS.md`.

## Exit criterion

| Check | Result |
|---|---|
| Clean install | The dmg was installed into a scratch Applications folder, with the Swift build products hidden: nothing outside the bundle was needed. **Simulated**: not a second, fresh Mac |
| Networking disabled | The bundled semd ran under `sandbox-exec` with all IP traffic denied. It indexed 7/7 synthetic files with 0 failures and answered queries. The model loaded from the bundle in 1.72 s, and no network activity appeared in the log |
| LaunchAgent | The bundled UI installed semd as an agent (test label). After `kill -9`, launchd restarted it in 11 s; `semctl agent restart` also worked; uninstall left nothing behind |
| Signing | **Ad hoc only** (no Developer ID on this Mac). `codesign --verify --strict --deep` passes; `spctl` rejects the app, as expected |
| Notarization | **Not done.** It is scripted and runs only when `NOTARY_PROFILE` and a real `SIGN_ID` are set |

Queries (offline, from the installed bundle):

| Query | Top hit | Score |
|---|---|---|
| when is my dentist appointment | voicemail.wav `0:00–0:04` (then dentist.txt 0.701) | 0.731 |
| how much is the rent | lease.pdf p.1 | 0.722 |
| orange sky with the sun going down | sunset.png | 0.747 |
| soup recipe | notes/soup.txt | 0.735 |

```sh
packaging/build.sh                    # → packaging/out/SemSearch.app (1.1 GB), SemSearch-0.1.0.dmg (596 MB)
SIGN_ID="Developer ID Application: … (TEAM)" NOTARY_PROFILE=semsearch packaging/build.sh
packaging/uninstall.sh [--purge]      # stop + remove the agent; --purge also deletes index and logs
SEMSEARCH_LAUNCHD=1 go test -run TestLaunchd ./internal/agent   # real launchd, test label
```

## Bundle layout

```
SemSearch.app/Contents/
  MacOS/SemSearch          UI (Go, MyGo, no cgo)
  Helpers/semd             daemon (cgo for FSEvents)
  Helpers/semctl           CLI
  Helpers/semd-embed       Swift MLX sidecar
  Helpers/*.bundle         SwiftPM resources (MLX metallib, Crypto, Hub)
  Resources/models/        text-q8, vision-q8, audio-bf16
  Info.plist               dev.semsearch.app, macOS 14.0+, arm64
```

- semd without `-model` uses `../Resources/models` next to its own executable,
  and finds `semd-embed` in its own folder. If no models are bundled it fails
  loud.
- SwiftPM looks up its resource bundles next to the executable, so they sit in
  `Helpers/`.
- Code is signed inside out with the hardened runtime: bundles, helpers (with
  `dev.semsearch.*` identifiers), then the app.
- The dmg is made with `diskutil image create from` (ULFO). `hdiutil create`
  failed with "Resource busy" and is deprecated from macOS 26; it stays only
  as a fallback.

## LaunchAgent (`internal/agent`, stdlib only)

- Label `dev.semsearch.semd`; plist at `~/Library/LaunchAgents/<label>.plist`.
  It runs `Helpers/semd -state ~/Library/Application Support/SemSearch`, logs
  to `~/Library/Logs/SemSearch/<label>.log`, and uses `RunAtLoad`,
  `KeepAlive{SuccessfulExit: false}` and `ThrottleInterval` 10. With
  `AssociatedBundleIdentifiers`, System Settings › Login Items shows it under
  SemSearch.
- The plist is hand-built and deterministic, with all values validated and
  escaped. It carries `SEMSEARCH_SEMD_SHA256`, so a new semd binary changes
  the plist.
- `Ensure` does nothing when the plist is current and the agent is loaded.
  Otherwise it does bootout → write atomically → bootstrap (with retries).
  The UI calls it at every launch, so an app update or a manual
  `launchctl bootout` is repaired on the next start.
- `bootout` is asynchronous: `Uninstall`/`Install` poll until launchd lets
  go of the service, and fail loud after 30 s.
- The UI refuses to install the agent when it runs from the dmg (read-only
  volume) or translocated, and asks the user to move the app to
  Applications first. `-agent=false` turns this off (development: run semd
  by hand).
- CLI: `semctl agent install|uninstall|restart|status`.

## TCC / Full Disk Access

- semd is the process that reads files, so semd (not the app) needs access
  to protected folders (Desktop, Documents, Downloads, Mail and so on).
- A root that fails with EPERM shows in the status line ("1 folder
  unreadable"). Settings then shows a **Full Disk Access** section with
  steps and three buttons: Open Privacy Settings (deep link), Show semd in
  Finder (to drag into the list) and Restart semd.
- semd retries unreadable roots every minute. While a root is unreadable it
  does not save the watch checkpoint, so after a restart it rescans instead
  of replaying (replaying could skip the root). The retry logs only when the
  error changes.
- **Not verified for real:** no TCC prompt or grant was made on this Mac.
  The flow was tested with `chmod 0` roots (and fakes in the UI tests).
  Ad hoc grants are tied to the cdhash, so every rebuild needs a new grant;
  Developer ID signing fixes that.

## Not done

- **Developer ID signing and notarization.** There is no identity on this
  Mac. `build.sh` does both when `SIGN_ID`/`NOTARY_PROFILE` are set, but this
  path has never run. Until then Gatekeeper blocks the dmg on other Macs
  (right click › Open, or `xattr -d com.apple.quarantine`).
- **A really fresh Mac.** The clean install was simulated on the build
  machine (scratch Applications, hidden build products, IP denied with
  `sandbox-exec`). Quarantine and translocation of a downloaded dmg, and
  Gatekeeper's first-launch dialog, were not exercised.
- **Real TCC.** Whether a Full Disk Access grant reaches the running semd
  without a restart is unknown, hence the Restart button.
- **Size.** The app is 1.1 GB and the dmg 596 MB, almost all models.
  Downloading models on first run would conflict with "works with
  networking disabled", so they stay bundled.
- **Updates.** There is no updater; a new dmg replaces the app, and the next
  launch re-installs the agent (the hash changes).
- From M6, still open: opening audio/video at the hit's time.

## Live status (follow-up)

The UI keeps semd's SSE stream (`/v1/events`) open and no longer polls
every 2 s while it is up.

- An event only marks the status stale. The view then fetches
  `/v1/status` at most every 250 ms, so a burst of thousands of `indexed`
  events costs a few requests (the test sends 500: one request). The
  stream goroutine posts at most one mark at a time to the main thread.
- While the stream is open, a 30 s poll stays as a safety net. If the
  stream drops, the UI polls every 2 s again (and at once) and reconnects
  with backoff from 1 s to 30 s. A stream error is logged once, not at
  every retry.
- The client makes a synthetic first `open` event, sent once semd has
  subscribed. The UI fetches the status then, as events before it were
  missed. `semctl events` does not print it.
- semd now publishes `pause` events (the reasons, or `resumed`) whenever
  the set of pause reasons changes: manual, battery, Low Power Mode or
  thermal. Status fields that change without an event (pause, from
  `semctl` or the power policy) would otherwise wait for the 30 s poll.
- Open settings reload the failure list whenever the failed count changes.

Tests: `TestLiveStatus` (coalescing, no polling while live, the fallback
and reconnect), `TestFailuresFollowStatus`, `TestQueueOnPause`, and
`TestEventsStream`, which now also checks `open` and `pause`.
`TestRealSemd` opens the stream against a running semd and sees a pause
appear through it.

## Menu bar and shortcut (follow-up)

SemSearch now lives in the menu bar. Closing the window hides it instead
of quitting.

- The menu-bar icon is drawn in code: a 32x32 template image of a page
  under a magnifier, so it follows the light and dark menu bar. Its menu
  has these items:
  - Search Files
  - Settings…
  - the index status (disabled, same text as the window's status line)
  - Pause/Resume Indexing (disabled while semd is unreachable)
  - Open at Login (a checkbox, through MyGo's `SetOpenAtLogin`; on error
    it reverts the check and shows a notice)
  - Quit SemSearch
- A global shortcut toggles the window: it hides the window if it is
  focused, and shows and focuses it otherwise. The default is
  Option-Shift-Space; change it with `-shortcut`, or pass `-shortcut ""`
  for none. If registration fails (e.g. the keys are taken), the app logs
  it, shows a notice and runs without a shortcut.
- Escape clears the query, and a second Escape hides the window. Started
  at login, the window begins hidden. A second launch or a Dock click
  shows it.
- Quitting: MyGo's quit closes every window, and a prevented close cancels
  the quit. The close handler therefore hides only when no quit is in
  progress (`OnBeforeQuit` sets a flag). SIGTERM quits both a visible and
  a hidden app.
- A hidden window draws no frames, so polling driven by the view stopped.
  Status scheduling moved into `pollStatus`, and a 2 s ticker calls it
  while the window is hidden. A pause then reaches the menu within about
  2 s.
- Tests: `TestEscapeHides`, `TestMenuStatus`, `TestTrayIcon`. The real
  app was started: the icon and the default shortcut registered without
  errors, and quitting worked.
- **Not verified:** the shortcut firing (synthesizing keys needs
  Accessibility), the menu's appearance (no screen capture permission
  here), and Open at Login.
- The clean-install E2E passes again. Step 1 now runs semd with
  `-power-policy=false`: on battery it had paused indexing, and the run
  timed out.

## Follow-up: search options, Activity, Service tab, Uninstall

- **Search options** (`ui/filters.go`): three menus sit under the search
  field. Kinds offers All Kinds, or any mix of Text, Office, PDF, Images,
  Audio and Video. Folder offers All Folders or one indexed root. Number
  of results offers 10, 25, 50 or 100, with 50 as the default. A Reset
  button appears while kinds or a folder narrow the results. Any change
  searches again at once. Choosing every kind counts as all kinds. When
  semd stops indexing the chosen folder, the filter falls back to All
  Folders.
- **Activity tab** (`ui/activity.go`): semd's events appear live, newest
  first, 8 per page. The UI keeps the last 500 in memory only. A queue
  batches bursts, so many events cost one redraw. Moving off page 1
  holds the log; Back to Live or Clear releases it. Problems (failed,
  fatal, lost, missed) show in red. The menu bar has an "Activity…"
  item.
- **Service tab** (`ui/service.go`, `internal/agent.Info/Stop`):
  - It shows semd's launchd state from `launchctl print`: state, pid,
    runs, last exit and program. It also shows the connection, the
    LaunchAgent label and the log. launchd is polled every 2 s, and only
    while this tab is open.
  - Buttons: Start, Stop (until the next login), Restart, Open Log, and
    Uninstall SemSearch…
  - Without the agent (a dev binary or `-agent=false`), the tab says to
    use semctl and launchctl instead.
- **Uninstall** (`ui/uninstall.go`): the user confirms first. While it
  runs, a sheet that cannot be dismissed shows progress. It then lists
  everything removed, or the error with a Try Again button. The steps,
  in order:
  1. Remove the Login Items entry.
  2. Boot out the LaunchAgent and delete its plist.
  3. Delete the state directory (index and settings).
  4. Delete `~/Library/Logs/SemSearch/*.log`.
  5. Delete the trail macOS keeps by bundle ID: Preferences plist,
     Caches, Saved Application State, HTTPStorages (both forms), WebKit,
     and `$(getconf DARWIN_USER_CACHE_DIR)/<id>`, which holds Metal
     shader caches.
  6. Run `defaults delete` and `tccutil reset All <id>`.
  7. Move SemSearch.app to the Trash.
  8. A detached `/bin/sh` sweeper waits for the app to exit, then
     deletes the prefs and trail again, because the app may rewrite them
     while quitting.

  Guards, all checked before anything changes:
  - Paths must be clean and absolute.
  - The state directory must not be `/`, home or an ancestor of home. It
    must be a real directory, not a symlink, and hold only entries semd
    makes.
  - The app must end in `.app`, and its Info.plist must carry the bundle
    ID.
  - The index is deleted only under semd's own flock, which proves no
    semd still uses it.
  - Every step is idempotent, so Try Again resumes where a failure
    stopped.

  **Not automatic:** semd's own Full Disk Access entry. `tccutil` takes
  only bundle IDs, so the final screen sends the user to Privacy
  Settings.
- `packaging/uninstall.sh --purge` now removes the same trail and runs
  the same `defaults` and `tccutil` steps. It has been checked with
  `bash -n` only.
- **Sheet clipping fix:** MyGo's Modal pads its panel by 20 and caps it
  at the window size minus 40, so at the minimum window size the old
  settings sheet was clipped. The sheet is now derived from the window
  minimum (600×400), and `minWindowH` is 508. `TestSettingsFit` now
  computes the real geometry and fails on any clipped control in all 7
  tabs.
- Tests:
  - `TestSearchOptions`, `TestActivity`, `TestService`,
    `TestUninstallFlow`.
  - `TestUninstaller` and `TestUninstallerRefuses` run against fakes and
    temp dirs.
  - `TestSweepScript` runs the real `/bin/sh` against temp paths.
  - `make test` passes.

  Nothing was run against the real install.

## Rename: SemSearch → Kim Quy

- Kim Quy is the Golden Turtle God, who keeps what lies in the depths and
  brings it back. The repository folder is now `kimquy`; the binaries stay `semd`
  and `semctl`, and the Go module path stays `semsearch` (internal only).
- New names:
  | What | New name |
  | --- | --- |
  | App | `Kim Quy.app` |
  | Executable | `KimQuy` |
  | dmg | `KimQuy-<version>.dmg` |
  | Bundle ID | `dev.kimquy.app` |
  | LaunchAgent | `dev.kimquy.semd` |
  | Signing identifiers | `dev.kimquy.semd`, `dev.kimquy.semd-embed`, `dev.kimquy.semctl` |
  | State | `~/Library/Application Support/Kim Quy` |
  | Logs | `~/Library/Logs/Kim Quy` |
- `packaging/migrate-from-semsearch.sh` (a one-off for the developer's
  own install; not published) moves an existing install. It
  stops the old agent, moves the index, deletes SemSearch's trail and
  puts SemSearch.app in the Trash. Before changing anything it refuses
  if:
  - SemSearch is running;
  - Kim Quy's agent is loaded;
  - Kim Quy's state folder already exists;
  - a semd still serves the old index.

  It was tested against a fake home with fake labels.
- Full Disk Access and Login Items cannot be moved: macOS ties them to
  the old binary and bundle. Grant Full Disk Access to the new semd
  again.

## Slim app: the model is downloaded, not bundled

- The app was 1.1 GB, almost all of it the model. `make app` now builds a
  slim app by default: 43 MB, and a 15 MB dmg. `BUNDLE_MODELS=1` still
  bundles `MODELS` (a full app works offline from the start).
- `internal/model` pins the model to one revision of the Hugging Face
  repo `Nurymanau/EmbeddingGemma-2-MLX-Swift`. That repo is not gated,
  is Apache-2.0, and holds the same bytes as `models/release`. For every
  file it keeps the size and sha256. The parts are:
  | Part | Size | For |
  | --- | --- | --- |
  | `text-q8` | 320 MB | text and documents (required) |
  | `vision-q8` | 193 MB | images, PDFs and video frames |
  | `audio-bf16` | 611 MB | audio |
- Where semd looks: the bundle's `Contents/Resources/models` if it has
  `text-q8`, otherwise `<state>/models`. Without `text-q8`, or with a
  damaged part (checked by size), semd exits with "open Kim Quy to
  download it".
- Downloads:
  - files are staged under `models/.staging`;
  - an interrupted download resumes with `Range`, and restarts if the
    server ignores it;
  - a download that receives nothing for 60 s fails;
  - free space is checked first;
  - each file is checked against its sha256;
  - a part is renamed into place only when whole, because semd-embed takes
    a part to be there once its `manifest.json` is.
- "Use Files I Have…" takes a model folder, one of its part folders or an
  old app. Files are copied as APFS clones (`cp -c`, so they take no
  extra space), falling back to a plain copy, and then hash-checked.
- The UI:
  - At first launch it shows a setup screen instead of search: Text
    (required), Images (on by default) and Audio (off), with sizes, the
    total, free disk space, Download or Use Files I Have…, progress and
    Stop. When that finishes it starts semd; until then the LaunchAgent is
    not installed.
  - A new Settings tab, Model, shows the location and each part's state,
    with Add… (after a confirmation) and Show in Finder.
  - Adding a part changes the model id, so semd restarts and the index
    must be rebuilt. The tab says so until Rebuild Index runs.
  - To fit eight tabs, two are shorter: "Exclusions" is now "Excluded"
    and "Failures" is now "Failed".
- Uninstall also deletes `<state>/models`.
- `migrate-from-semsearch.sh` clones the old app's bundled model into
  the new state, so there is no download and the index stays valid.
  `OLD_APP=` points it at the old app when the old LaunchAgent's path is
  stale (as it is here, after the folder rename).
- Checked:
  - unit tests with a fake CDN (redirects, resume, a server ignoring
    `Range`, a bad `Content-Range`, a bad hash, HTTP errors, a stall,
    cancel, a damaged part, the lock) and import tests;
  - UI tests for the setup screen, the Model tab and the settings fit;
  - a real `text-q8` download from Hugging Face, stopped at 105 MB and
    resumed; it is byte-identical to `models/release`
    (`KIMQUY_REAL_DOWNLOAD=<dir> go test -run TestRealDownload ./internal/model`);
  - semd loading it from `<state>/models` with no `-model`.

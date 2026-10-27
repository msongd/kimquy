# M5 results: watching for changes, power policy

Measured 2026-10-26 on the same machine as `M0-RESULTS.md`.

## Exit criterion

| Check | Result |
|---|---|
| Edit to an indexed file is searchable | 35 ms (engine test); 50 ms through `semd` + `semctl` |
| New file in a newly created directory is searchable | 0.5 s |
| Deleted file is gone from the index | 0.5 s |
| Change made while semd was down, after restart | indexed 50–70 ms after start, by replay |
| Full scans requested on restart | 0 |

Real FSEvents in `internal/engine/watch_test.go` (`TestWatchFSEvents`) and
`internal/watch/watch_test.go`; fake embedder, so the times are the
watcher's, not the model's. With the real model, add the embedding time.
M0 measured about 19 chunks/s warm, so a typical edited document adds well
under a second, plus about 2 s if the sidecar has to load the model again. FSEvents delivers the first event
of a quiet period at once (`NoDefer`). Later events in a burst are grouped
for 0.5 s.

## How it works

- `internal/watch`: an FSEvents stream (file-level events, `WatchRoot`) via
  cgo. The callback hands events to a bounded buffer and never blocks. If
  the buffer fills (100k events), a full rescan is triggered.
- The engine saves `watch.json`: the last event ID, the roots, and each
  root's FSEvents history UUID. It saves only when everything up to that ID
  is indexed: no scan pending or running and the queue empty. A crash
  therefore replays changes rather than losing them.
- At startup, if the roots and history UUIDs match, the stream starts from
  the saved ID and no scan runs. Catalog entries left pending are requeued.
  Otherwise the engine watches from "now" and does one full scan.
- Events:

  | Event | Action |
  |---|---|
  | File (including symlink) | queued if it is in the catalog or could be indexed |
  | Directory created, removed or renamed; `MustScanSubDirs` | scan of that subtree only |
  | Dropped events, `RootChanged`, mount or unmount | full scan |

- Changing roots restarts the stream from "now" and rescans.
- Transient failures are requeued before the worker releases the path. The
  queue is therefore never momentarily empty while a retry is pending, which
  could otherwise let a checkpoint skip it.
- `semd -watch=false` restores the M2 behaviour: a full scan at every start.
  Status reports `watch`: `off`, `idle` (no roots), `replaying` or `live`.

## Power policy

`sched.PowerReasons` is a pure function of battery, Low Power Mode and
thermal state. `sched.PowerMonitor` polls every 10 s and pauses the queue
with the matching reasons:
- `on battery`, when `pause_on_battery` is set (the default);
- `low power mode`;
- `thermal state serious or worse`.

Reasons show in `semctl status` under `paused`. The probe (`internal/power`)
reads IOKit's providing power source and `NSProcessInfo`, through cgo in
semd itself.

**Deviation from the plan:** the plan had the sidecar report these. Polling
the sidecar would keep it from idling out (it exits after its towers
unload), so semd reads them directly. A probe error stops semd, since
indexing would otherwise ignore power. `-power-policy=false` turns the policy
off.

Tests cover each reason, resuming, and config changes. Pausing on battery
was verified with a fake probe; the real probe was only exercised on AC at
nominal thermal state.

## Not yet done

- **On-battery and thermal behaviour on real hardware.** Unplugging, Low
  Power Mode, and a heat soak have not been tried with a running semd.
- **Volumes without event history** (some network and FAT volumes).
  `History` fails for them, so nothing is saved and every start does a full
  scan. This is logged, but there is no polling fallback, so changes made
  while semd runs go unnoticed there.
- **Roots missing at startup** (an unplugged disk) likewise force a full scan
  on each start until they come back.
- **Power draw.** Idle watching should be close to zero, since FSEvents is
  push-based and the only timers run every 2 s and 10 s. This still needs
  `sudo powermetrics`, as for M0/M4.

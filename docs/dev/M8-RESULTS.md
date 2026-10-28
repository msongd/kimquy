# M8 results: Quick view pane

Built 2026-10-28 on the same machine as `M0-RESULTS.md`.

## Exit criterion

| Check | Result |
|---|---|
| Preview at the match | Text, code and Office files show up to 64 KB of text, centered on the hit's byte range and cut at line ends. The match is highlighted and scrolled into view. A PDF shows the hit's page, with the match highlighted when the hit has a range |
| Every kind | Images show their picture (see Thumbnails). Audio and video show the matched time span and their duration. A PDF page that was searched as an image says so instead of showing text |
| Details | Folder, match location, size, pages / duration / dimensions, modified, indexed (with the status if it is not `indexed`), fragments (vectors) and the SHA-256 prefix. Open and Show in Finder sit below |
| Follows the list | The pane reloads when the chosen row changes (↑↓, a click, new results). A reply for a row the user has already left is dropped (tested with replies out of order) |
| Smallest window | At 680×480 the pane, its buttons and the eye on each row fit inside the window (tested; screenshot checked) |

```sh
semctl preview ~/Documents/lease.pdf page:2        # text of page 2, then the details
semctl preview ~/notes/long.txt bytes:120400-120470  # a locator as semctl search prints it
cd ui && SEMSEARCH_SHOT=/abs/prefix go test -run QuickLookFits   # → prefix-quicklook.png
```

## How it works

- **semd reads the file, not the UI.** semd has Full Disk Access, and the UI
  holds no state (§6). Two new endpoints:
  - `POST /v1/preview` `{path, locator?}` returns the text window and the
    details.
  - `GET /v1/thumbnail?path=` returns the image bytes.

  Both serve only files in the catalog. A path that is not indexed gets 404.
  A relative or unclean path, or a locator that does not fit the file's kind
  (a page in an image, a byte range in a PDF), gets 400.
- **The text is extracted the way indexing does it**: `readStable` and
  `extractText` with the same size limits. A PDF asks the sidecar for one
  page at query priority.
- **The sidecar gets 5 s.** If it is busy indexing, the preview still comes
  back with its details, and the text area says "the embedder is busy
  indexing; try again". A preview never fails just because the model is
  busy.
- **The file changed or is gone.** The details come from the catalog. If the
  file is gone, the preview is marked stale and says why there is no text.
  If its size or modification time differs from the indexed one, the pane
  warns that the match may be off.
- **Thumbnails.**
  - PNG, JPEG and GIF up to 4 MB and 2048 px go to the UI as they are. So do
    WebP and BMP up to 2 MB, which Go's standard library cannot measure.
  - Everything else (HEIC, TIFF, larger photos) is scaled by macOS's own
    `/usr/bin/sips` to a JPEG of at most 1600 px.
  - No new dependencies: `sips` ships with macOS, and semd stays stdlib only.
- **The client fails loud.** `client.Preview` rejects a reply whose match
  range lies outside its text. The UI's `splitMatch` panics on a bad range.
- **The UI.**
  - An eye on each row opens the pane on that row, or closes it if it
    already shows that row. ⌘I does the same for the chosen row.
  - The pane is a `SplitPercent` (52/48 to start, resizable).
  - Loading and image decoding run off the main thread. Each load has a
    sequence number, and the previous load is canceled.
  - The text is drawn in three parts: lines before the match, the match's
    lines as `RichText` with a highlighted span, and lines after. Only the
    middle part is scrolled into view, once per preview. Text files use
    monospace, and the text can be selected.

## Not done

- **⌘Y.** Finder's and Spotlight's Quick Look key is taken by MyGo's text
  input as redo, before any app shortcut sees it. The search field has focus
  almost all the time, so the pane uses ⌘I (Finder's Get Info) instead.
- **Space to preview**, as in Finder. Space types into the search field.
- **Playback.** Audio and video show the matched span; Open plays the file.
  There is no player or poster frame in the pane.
- **Office layout.** Office files show their extracted text, not a rendered
  page. The same goes for PDFs: the page's text, not its picture.
- **Text beyond 64 KB** around the match is not shown. "…" marks a cut at
  either end, and Open shows the whole file.
- **Against the installed app.** The tests use a fake semd (UI), and the
  real engine with the fake embedder (engine, API). The installed semd
  predates `/v1/preview`, so the pane was not driven against it. Real
  `sips` was checked by hand (HEIC → 400 px JPEG).

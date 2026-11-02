# M8 results: Quick view pane

Built 2026-10-28 on the same machine as `M0-RESULTS.md`.

## Exit criterion

| Check | Result |
|---|---|
| Preview at the match | Text, code and Office files show up to 64 KB of text, centered on the hit's byte range and cut at line ends. The match is highlighted and scrolled into view. A PDF shows the hit's page as a picture with the match highlighted in yellow, or (Page/Text switch) the page's text with the match highlighted |
| Every kind | Images show their picture (see Thumbnails). Audio and video show the matched time span and their duration. A PDF page that was searched as an image says so instead of showing text |
| Details | Folder, match location, size, pages / duration / dimensions, modified, indexed (with the status if it is not `indexed`), fragments (vectors) and the SHA-256 prefix. Open and Show in Finder sit below |
| Follows the list | The pane reloads when the chosen row changes (↑↓, a click, new results). A reply for a row the user has already left is dropped (tested with replies out of order) |
| Keys | ⌘I opens and closes the pane anywhere. Space does it while the list has the focus (a click on a row); in the search field Space types a space (tested) |
| Smallest window | At 680×480 the pane, its buttons, the Page/Text switch and the eye on each row fit inside the window (tested; screenshot checked) |

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
  - `GET /v1/thumbnail?path=&locator=` returns the image bytes, or a PDF
    page's picture.

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
- **PDF page pictures.** `sips` draws only a PDF's first page, so the
  sidecar renders pages: a new op, `render_pdf_page {path, from, match_start,
  match_end}`, which bumps the sidecar protocol to 2.
  - The sidecar draws the page as for embedding (crop box, rotation, 1536 px
    on the longest side). It then fills each line of the match in yellow,
    multiplied, so the text stays dark. It finds the lines with PDFKit's
    `selection(for:)` on the UTF-8 range mapped to UTF-16, and the result
    is a PNG of at most 8 MB.
  - A range that does not fall on whole characters of the page's text (the
    file changed) is drawn without a highlight. A page past the end is
    `invalid`, which the API reports as 422.
  - semd checks the reply: it decodes the PNG header, and checks the page
    count and the size. A bad picture is a protocol violation and restarts
    the sidecar, as for any reply.
  - It runs at query priority within the same 5 s budget. A busy embedder
    gives 503 and the pane says so; the text view still works.
  - The real model test (`TestModelPDF`) renders a page plain and with a
    highlight: 1–5 % of the pixels differ, and a range past the text
    changes nothing. The picture was also checked by eye.
- **The client fails loud.** `client.Preview` rejects a reply whose match
  range lies outside its text. The UI's `splitMatch` panics on a bad range.
- **The UI.**
  - An eye on each row opens the pane on that row, or closes it if it
    already shows that row. ⌘I does the same for the chosen row.
  - The pane is a `SplitPercent` (52/48 to start, resizable).
  - Loading and image decoding run off the main thread. Each load has a
    sequence number, and the previous load is canceled.
  - A PDF opens on its page picture, fitted to the pane's width. A Page/Text
    switch in the header shows the text instead, and the choice holds for
    the next PDF. After a click on it the search field takes the focus
    back, so ↑↓ still choose results rather than flip the switch.
  - Space is bound on the list only. Opening the pane rebuilds the list
    inside the split, which drops its focus, so the list takes it back and
    Space can close what it opened.
  - The text is drawn in three parts: lines before the match, the match's
    lines as `RichText` with a highlighted span, and lines after. Only the
    middle part is scrolled into view, once per preview. Text files use
    monospace, and the text can be selected.

## Not done

- **⌘Y.** Finder's and Spotlight's Quick Look key is taken by MyGo's text
  input as redo, before any app shortcut sees it. The search field has focus
  almost all the time, so the pane uses ⌘I (Finder's Get Info) instead.
  Space works only from the list, since in the field it types.
- **Scrolling the page picture to the match.** The page is fitted to the
  pane's width, so a match low on the page needs a scroll. The text view
  does scroll to it.
- **Playback.** Audio and video show the matched span; Open plays the file.
  There is no player or poster frame in the pane.
- **Office layout.** Office files show their extracted text, not a rendered
  page.
- **Text beyond 64 KB** around the match is not shown. "…" marks a cut at
  either end, and Open shows the whole file.
- **Against the installed app.** The tests use a fake semd (UI), and the
  real engine with the fake embedder (engine, API). The real sidecar's
  `render_pdf_page` is tested against the release model. The installed
  semd predates `/v1/preview`, so the pane was not driven against it. Real
  `sips` was checked by hand (HEIC → 400 px JPEG).

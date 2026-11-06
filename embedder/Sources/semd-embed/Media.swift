// Media decoding for semd-embed: images via ImageIO, PDFs via PDFKit. The
// limits mirror internal/embed/media.go.

import CoreGraphics
import Foundation
import ImageIO
import PDFKit

let maxPDFPagesPerCall = 8
let maxPDFPageText = 512 << 10
let pdfRenderMinChars = 32
/// Vision budget per image: the larger of the two the model takes (70, 140).
let imageSoftTokens = 140
/// Vision budget per video frame: the encoder's smallest that keeps the
/// scene recognizable, so 32 frames cost about as much as 8 photos.
let frameSoftTokens = 70
/// Images and PDF pages are decoded straight to this longest side, already
/// more than 140 soft tokens see; LiteRT resizes from there.
let decodeMaxSide = 1024
/// Pages for the quick view get this longest side.
let pdfRenderMaxSide = 1536
/// Larger images are refused before decoding: a decompression bomb guard.
let maxSourcePixels = 200_000_000

/// A request failure with its protocol error code.
struct Failure: Error {
    let code: String
    let message: String
    static func invalid(_ m: String) -> Failure { Failure(code: "invalid", message: m) }
    static func `internal`(_ m: String) -> Failure { Failure(code: "internal", message: m) }
}

/// Mirrors embed.TooLittleText: fewer than pdfRenderMinChars non-space
/// code points (Go's unicode.IsSpace is the White_Space property).
func tooLittleText(_ s: String) -> Bool {
    var n = 0
    for c in s.unicodeScalars where !c.properties.isWhitespace {
        n += 1
        if n >= pdfRenderMinChars { return false }
    }
    return true
}

/// Draws into an opaque white sRGB canvas, so transparency ends up
/// flattened onto white.
func bitmap(width w: Int, height h: Int, draw: (CGContext) -> Void) throws -> CGImage {
    guard w > 0, h > 0, w <= 20000, h <= 20000, w * h <= 40_000_000 else {
        throw Failure.invalid("bitmap \(w)×\(h) out of range")
    }
    guard let srgb = CGColorSpace(name: CGColorSpace.sRGB) else { throw Failure.internal("no sRGB color space") }
    guard let ctx = CGContext(
        data: nil, width: w, height: h, bitsPerComponent: 8, bytesPerRow: 0,
        space: srgb, bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue)
    else { throw Failure.invalid("cannot allocate a \(w)×\(h) bitmap") }
    ctx.setFillColor(red: 1, green: 1, blue: 1, alpha: 1)
    ctx.fill(CGRect(x: 0, y: 0, width: w, height: h))
    ctx.interpolationQuality = .high
    draw(ctx)
    guard let img = ctx.makeImage() else { throw Failure.internal("cannot finish a \(w)×\(h) bitmap") }
    return img
}

/// Decodes the primary image of a file (JPEG, HEIC, PNG, …) with its EXIF
/// orientation applied, downsampled to decodeMaxSide.
func loadImage(_ path: String) throws -> CGImage {
    let noCache = [kCGImageSourceShouldCache: false] as CFDictionary
    guard let src = CGImageSourceCreateWithURL(URL(fileURLWithPath: path) as CFURL, noCache),
          CGImageSourceGetCount(src) >= 1
    else { throw Failure.invalid("not a readable image") }
    let index = CGImageSourceGetPrimaryImageIndex(src)
    guard let props = CGImageSourceCopyPropertiesAtIndex(src, index, noCache) as? [CFString: Any],
          let w = props[kCGImagePropertyPixelWidth] as? Int, let h = props[kCGImagePropertyPixelHeight] as? Int,
          w > 0, h > 0
    else { throw Failure.invalid("image has no dimensions") }
    guard w <= 100_000, h <= 100_000, w * h <= maxSourcePixels else {
        throw Failure.invalid("image is \(w)×\(h), over \(maxSourcePixels / 1_000_000) megapixels")
    }
    let opts: [CFString: Any] = [
        kCGImageSourceCreateThumbnailFromImageAlways: true,
        kCGImageSourceCreateThumbnailWithTransform: true,
        kCGImageSourceThumbnailMaxPixelSize: decodeMaxSide,
        kCGImageSourceShouldCacheImmediately: true,
    ]
    guard let img = CGImageSourceCreateThumbnailAtIndex(src, index, opts as CFDictionary) else {
        throw Failure.invalid("cannot decode image")
    }
    let rect = CGRect(x: 0, y: 0, width: img.width, height: img.height)
    return try bitmap(width: img.width, height: img.height) { $0.draw(img, in: rect) }
}

/// Renders a page's crop box, honoring its rotation, at decodeMaxSide.
func renderPage(_ page: CGPDFPage) throws -> CGImage {
    let box = page.getBoxRect(.cropBox)
    let quarter = (Int(page.rotationAngle) % 360 + 360) % 180 != 0
    let pw = quarter ? box.height : box.width, ph = quarter ? box.width : box.height
    guard pw >= 1, ph >= 1 else { throw Failure.invalid("page has an empty crop box") }
    let scale = CGFloat(decodeMaxSide) / max(pw, ph)
    let w = max(1, Int((pw * scale).rounded())), h = max(1, Int((ph * scale).rounded()))
    // getDrawingTransform never scales up, so map at 1:1 and scale ourselves.
    let t = page.getDrawingTransform(.cropBox, rect: CGRect(x: 0, y: 0, width: pw, height: ph), rotate: 0, preserveAspectRatio: true)
    return try bitmap(width: w, height: h) { ctx in
        ctx.scaleBy(x: scale, y: scale)
        ctx.concatenate(t)
        ctx.clip(to: box)
        ctx.drawPDFPage(page)
    }
}

/// Mirrors embed.MaxPDFPicture.
let maxPDFPicture = 8 << 20

/// The UTF-16 range of the UTF-8 byte range [start, end) of text, or nil
/// when it does not name whole code points of the text (the file changed).
func utf16Range(of text: String, start: Int, end: Int) -> NSRange? {
    let u = text.utf8
    guard start >= 0, start < end, end <= u.count else { return nil }
    let s = u.index(u.startIndex, offsetBy: start), e = u.index(u.startIndex, offsetBy: end)
    guard s.samePosition(in: text.unicodeScalars) != nil, e.samePosition(in: text.unicodeScalars) != nil else {
        return nil
    }
    return NSRange(s..<e, in: text)
}

/// Renders a page for the quick view as renderPage does, highlights the
/// lines of the selection, and encodes the picture as PNG.
func renderPagePNG(_ page: PDFPage, highlight: PDFSelection?) throws -> Data {
    guard let ref = page.pageRef else { throw Failure.invalid("cannot render the page") }
    let box = ref.getBoxRect(.cropBox)
    let quarter = (Int(ref.rotationAngle) % 360 + 360) % 180 != 0
    let pw = quarter ? box.height : box.width, ph = quarter ? box.width : box.height
    guard pw >= 1, ph >= 1 else { throw Failure.invalid("page has an empty crop box") }
    let scale = CGFloat(pdfRenderMaxSide) / max(pw, ph)
    let w = max(1, Int((pw * scale).rounded())), h = max(1, Int((ph * scale).rounded()))
    guard w <= pdfRenderMaxSide, h <= pdfRenderMaxSide,
          let srgb = CGColorSpace(name: CGColorSpace.sRGB),
          let ctx = CGContext(data: nil, width: w, height: h, bitsPerComponent: 8, bytesPerRow: 0,
                              space: srgb, bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue)
    else { throw Failure.invalid("cannot allocate a \(w)×\(h) bitmap") }
    ctx.setFillColor(red: 1, green: 1, blue: 1, alpha: 1)
    ctx.fill(CGRect(x: 0, y: 0, width: w, height: h))
    ctx.interpolationQuality = .high
    let t = ref.getDrawingTransform(.cropBox, rect: CGRect(x: 0, y: 0, width: pw, height: ph), rotate: 0, preserveAspectRatio: true)
    ctx.scaleBy(x: scale, y: scale)
    ctx.concatenate(t)
    ctx.clip(to: box)
    ctx.drawPDFPage(ref)
    if let highlight {
        // Multiplied, so the text stays dark on a yellow marker.
        ctx.setBlendMode(.multiply)
        ctx.setFillColor(red: 1, green: 0.84, blue: 0.2, alpha: 1)
        for line in highlight.selectionsByLine() {
            let r = line.bounds(for: page)
            if !r.isEmpty { ctx.fill(r.insetBy(dx: -1.5, dy: -1)) }
        }
    }
    guard let img = ctx.makeImage() else { throw Failure.internal("cannot finish the page picture") }
    let data = NSMutableData()
    guard let dest = CGImageDestinationCreateWithData(data, "public.png" as CFString, 1, nil) else {
        throw Failure.internal("no PNG encoder")
    }
    CGImageDestinationAddImage(dest, img, nil)
    guard CGImageDestinationFinalize(dest) else { throw Failure.internal("cannot encode the page picture") }
    guard data.length <= maxPDFPicture else {
        throw Failure.invalid("page picture is \(data.length) bytes, cap \(maxPDFPicture)")
    }
    return data as Data
}

/// Keeps the last opened PDF: semd reads a document in runs of pages.
/// Used by the worker only.
final class PDFCache: @unchecked Sendable {
    private struct Key: Equatable {
        let path: String
        let size: UInt64
        let mtime: Date
        let inode: UInt64
    }
    private var key: Key?
    private var doc: PDFDocument?

    func open(_ path: String) throws -> PDFDocument {
        let a: [FileAttributeKey: Any]
        do {
            a = try FileManager.default.attributesOfItem(atPath: path)
        } catch {
            throw Failure.invalid("stat: \(error.localizedDescription)")
        }
        guard let size = (a[.size] as? NSNumber)?.uint64Value, let mtime = a[.modificationDate] as? Date,
              let inode = (a[.systemFileNumber] as? NSNumber)?.uint64Value
        else { throw Failure.internal("stat \(path): missing attributes") }
        let k = Key(path: path, size: size, mtime: mtime, inode: inode)
        if k == key, let doc { return doc }
        drop()
        guard let d = PDFDocument(url: URL(fileURLWithPath: path)) else { throw Failure.invalid("not a readable PDF") }
        guard !d.isLocked else { throw Failure.invalid("PDF is encrypted") }
        guard d.pageCount >= 1 else { throw Failure.invalid("PDF has no pages") }
        guard d.pageCount <= 65535 else { throw Failure.invalid("PDF has \(d.pageCount) pages, over 65535") }
        key = k
        doc = d
        return d
    }

    func drop() {
        key = nil
        doc = nil
    }
}

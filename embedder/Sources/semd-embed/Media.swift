// Media decoding for semd-embed: images via ImageIO, PDFs via PDFKit. The
// limits mirror internal/embed/media.go.

import CoreGraphics
import EmbeddingGemma2
import Foundation
import ImageIO
import PDFKit

let maxPDFPagesPerCall = 8
let maxPDFPageText = 512 << 10
let pdfRenderMinChars = 32
/// Vision budget per image: the encoder's largest, ~800×800 pixels.
let imageSoftTokens = 280
/// Vision budget per video frame: the encoder's smallest that keeps the
/// scene recognizable, so 32 frames cost about as much as 8 photos.
let frameSoftTokens = 70
/// Images are decoded straight to this longest side; the encoder's exact
/// (slow, Pillow-compatible) resize then only has a small step left.
let decodeMaxSide = 2048
/// Rendered PDF pages get this longest side.
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

/// Draws into an opaque white sRGB canvas and returns its RGB pixels, so
/// transparency ends up flattened onto white.
func bitmap(width w: Int, height h: Int, draw: (CGContext) -> Void) throws -> RGBImage {
    guard w > 0, h > 0, w <= 20000, h <= 20000, w * h <= 40_000_000 else {
        throw Failure.invalid("bitmap \(w)×\(h) out of range")
    }
    guard let srgb = CGColorSpace(name: CGColorSpace.sRGB) else { throw Failure.internal("no sRGB color space") }
    var rgba = [UInt8](repeating: 0, count: w * h * 4)
    let ok = rgba.withUnsafeMutableBytes { p -> Bool in
        guard let ctx = CGContext(
            data: p.baseAddress, width: w, height: h, bitsPerComponent: 8, bytesPerRow: w * 4,
            space: srgb, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)
        else { return false }
        ctx.setFillColor(red: 1, green: 1, blue: 1, alpha: 1)
        ctx.fill(CGRect(x: 0, y: 0, width: w, height: h))
        ctx.interpolationQuality = .high
        draw(ctx)
        return true
    }
    guard ok else { throw Failure.invalid("cannot allocate a \(w)×\(h) bitmap") }
    var rgb = [UInt8](repeating: 0, count: w * h * 3)
    for i in 0..<(w * h) {
        rgb[i * 3] = rgba[i * 4]
        rgb[i * 3 + 1] = rgba[i * 4 + 1]
        rgb[i * 3 + 2] = rgba[i * 4 + 2]
    }
    return try RGBImage(width: w, height: h, bytes: rgb)
}

/// Decodes the primary image of a file (JPEG, HEIC, PNG, …) with its EXIF
/// orientation applied, downsampled to decodeMaxSide.
func loadImage(_ path: String) throws -> RGBImage {
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

/// Renders a page's crop box, honoring its rotation, at pdfRenderMaxSide.
func renderPage(_ page: CGPDFPage) throws -> RGBImage {
    let box = page.getBoxRect(.cropBox)
    let quarter = (Int(page.rotationAngle) % 360 + 360) % 180 != 0
    let pw = quarter ? box.height : box.width, ph = quarter ? box.width : box.height
    guard pw >= 1, ph >= 1 else { throw Failure.invalid("page has an empty crop box") }
    let scale = CGFloat(pdfRenderMaxSide) / max(pw, ph)
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

/// An encoder tower (vision or audio), loaded on first use and unloaded
/// when idle. Used by the worker only, so loading never races with encoding.
final class Tower: @unchecked Sendable {
    let name: String
    private let load: @Sendable () async throws -> Void
    private let drop: @Sendable () async -> Void
    private var loaded = false

    init(name: String, load: @escaping @Sendable () async throws -> Void, drop: @escaping @Sendable () async -> Void) {
        self.name = name
        self.load = load
        self.drop = drop
    }

    func ensure() async {
        guard !loaded else { return }
        let t0 = Date()
        do {
            try await load()
        } catch {
            die("load \(name) weights: \(error)")  // they passed the startup check: broken install
        }
        loaded = true
        log(String(format: "%@ loaded in %.2fs", name, Date().timeIntervalSince(t0)))
    }

    func unload() async {
        guard loaded else { return }
        await drop()
        loaded = false
        log("\(name) unloaded (idle)")
    }
}

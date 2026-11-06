// semd-embed: stateless embedding sidecar for semd.
//
// Speaks the NDJSON protocol specified in internal/embed/sidecar.go on
// stdin/stdout; logs go to stderr. One model (EmbeddingGemma 2 on LiteRT-LM,
// see Engine.swift), one serial worker: queries are taken before queued
// index work. Exits on stdin EOF, and with status 0 after an idle period
// (semd restarts it on demand).

import Foundation
import PDFKit

let protocolVersion = 2  // 2: render_pdf_page
let maxBatch = 32
let maxTokens = 8192

func log(_ s: String) {
    FileHandle.standardError.write(Data((s + "\n").utf8))
}

func die(_ s: String) -> Never {
    log("fatal: " + s)
    exit(1)
}

/// Mirrors embed.MaxLine.
let maxLine = 16 << 20

/// Splits the protocol stream on "\n" only. AsyncBytes.lines also breaks at
/// CR, NEL (U+0085), LS and PS, which JSON strings may carry unescaped, so
/// a text containing one would arrive cut in two.
struct RequestLines: AsyncSequence {
    typealias Element = Data
    let bytes: FileHandle.AsyncBytes

    struct AsyncIterator: AsyncIteratorProtocol {
        var it: FileHandle.AsyncBytes.AsyncIterator

        mutating func next() async throws -> Data? {
            var buf = Data()
            while let b = try await it.next() {
                if b == 0x0A { return buf }
                guard buf.count < maxLine else { die("request line over \(maxLine) bytes") }
                buf.append(b)
            }
            return buf.isEmpty ? nil : buf
        }
    }

    func makeAsyncIterator() -> AsyncIterator { AsyncIterator(it: bytes.makeAsyncIterator()) }
}

// MARK: options

struct Options {
    var model: URL
    var cache: URL?
    var dims: Int
    var idleExit: TimeInterval
}

func parseOptions() -> Options {
    var model: String?
    var cache: String?
    var dims = 256
    var idle: TimeInterval = 600
    var args = CommandLine.arguments.dropFirst()
    func value(_ flag: String) -> String {
        guard let v = args.popFirst() else { die("\(flag) needs a value") }
        return v
    }
    while let a = args.popFirst() {
        switch a {
        case "--model": model = value(a)
        case "--cache": cache = value(a)
        case "--dims":
            guard let d = Int(value(a)), [128, 256, 512, 768].contains(d) else { die("--dims must be 128, 256, 512 or 768") }
            dims = d
        case "--idle-exit":
            guard let s = TimeInterval(value(a)), s >= 0 else { die("--idle-exit must be seconds ≥ 0") }
            idle = s
        default: die("unknown argument \(a)")
        }
    }
    guard let model else { die("usage: semd-embed --model DIR [--cache DIR] [--dims 256] [--idle-exit SECONDS]") }
    if let cache, !cache.hasPrefix("/") { die("--cache must be an absolute path") }
    return Options(
        model: URL(fileURLWithPath: model, isDirectory: true),
        cache: cache.map { URL(fileURLWithPath: $0, isDirectory: true) }, dims: dims, idleExit: idle)
}

// MARK: protocol

struct Request: Decodable, Sendable {
    let id: UInt64
    let op: String
    var protocolVersion: Int? = nil
    var priority: String? = nil
    var texts: [String]? = nil
    var path: String? = nil
    var from: Int? = nil
    var count: Int? = nil
    var render: Int? = nil
    var startMS: Int? = nil
    var endMS: Int? = nil
    var timesMS: [Int]? = nil
    var matchStart: Int? = nil
    var matchEnd: Int? = nil
    enum CodingKeys: String, CodingKey {
        case id, op, priority, texts, path, from, count, render
        case protocolVersion = "protocol"
        case startMS = "start_ms", endMS = "end_ms", timesMS = "times_ms"
        case matchStart = "match_start", matchEnd = "match_end"
    }
}

/// Serializes whole lines onto stdout.
final class Output: @unchecked Sendable {
    private let lock = NSLock()
    func send(_ obj: [String: Any]) {
        guard let data = try? JSONSerialization.data(withJSONObject: obj) else { die("cannot encode response") }
        lock.lock()
        defer { lock.unlock() }
        FileHandle.standardOutput.write(data + Data([0x0A]))
    }
    func fail(_ id: UInt64, _ code: String, _ message: String) {
        send(["id": id, "error": ["code": code, "message": message]])
    }
}

/// Little-endian float32, base64: the wire format for vectors.
func encodeVector(_ v: [Float]) -> String {
    var d = Data(capacity: v.count * 4)
    for x in v {
        withUnsafeBytes(of: x.bitPattern.littleEndian) { d.append(contentsOf: $0) }
    }
    return d.base64EncodedString()
}

// MARK: scheduling

/// Pending work: queries first, then index requests, each FIFO.
actor Jobs {
    private var queries: [Request] = []
    private var index: [Request] = []
    private var waiter: CheckedContinuation<Request?, Never>?
    private var closed = false
    private(set) var busy = false
    private(set) var lastActive = Date()

    func push(_ r: Request) {
        lastActive = Date()
        if let w = waiter {
            waiter = nil
            busy = true
            w.resume(returning: r)
        } else if r.priority == "query" {
            queries.append(r)
        } else {
            index.append(r)
        }
    }

    func close() {
        closed = true
        if let w = waiter {
            waiter = nil
            w.resume(returning: nil)
        }
    }

    func next() async -> Request? {
        busy = false
        lastActive = Date()
        if !queries.isEmpty { busy = true; return queries.removeFirst() }
        if !index.isEmpty { busy = true; return index.removeFirst() }
        if closed { return nil }
        return await withCheckedContinuation { waiter = $0 }
    }

    func idleFor() -> TimeInterval {
        busy || !queries.isEmpty || !index.isEmpty ? 0 : Date().timeIntervalSince(lastActive)
    }
}

// MARK: main

/// Identifies the weights: the model id changes whenever they do, which
/// makes semd demand a reindex instead of mixing embedding spaces.
let modelID = "embeddinggemma-2-litert/" + modelSHA256.prefix(12)

/// Everything the worker needs; touched by the worker only.
struct Worker: Sendable {
    let engine: Engine
    let pdfs: PDFCache
    let out: Output

    func run(_ r: Request) async {
        do {
            out.send(try await handle(r))
        } catch let f as Failure {
            out.fail(r.id, f.code, f.message)
        } catch {
            out.fail(r.id, "internal", String(describing: error))
        }
    }

    private func handle(_ r: Request) async throws -> [String: Any] {
        switch r.op {
        case "embed_text": return try embedText(r)
        case "embed_image": return try embedImage(r)
        case "extract_pdf": return try extractPDF(r)
        case "render_pdf_page": return try renderPDFPage(r)
        case "probe_media": return try await probeMedia(r)
        case "embed_audio": return try await embedAudio(r)
        case "embed_frames": return try await embedFrames(r)
        default: throw Failure.internal("unknown op \(r.op)")
        }
    }

    private func embedText(_ r: Request) throws -> [String: Any] {
        guard let texts = r.texts, !texts.isEmpty else { throw Failure.invalid("no texts") }
        guard texts.count <= maxBatch else { throw Failure.invalid("batch of \(texts.count) exceeds \(maxBatch)") }
        // Prompts were applied by semd: encode as-is.
        return ["id": r.id, "vectors": try engine.embed(texts: texts).map(encodeVector)]
    }

    private func embedImage(_ r: Request) throws -> [String: Any] {
        guard let path = r.path, path.hasPrefix("/") else { throw Failure.internal("embed_image needs an absolute path") }
        return ["id": r.id, "vectors": [encodeVector(try engine.embed(image: try loadImage(path)))]]
    }

    private func probeMedia(_ r: Request) async throws -> [String: Any] {
        guard let path = r.path, path.hasPrefix("/") else { throw Failure.internal("probe_media needs an absolute path") }
        let m = try await probeAsset(path)
        return ["id": r.id, "duration_ms": m.durationMS, "audio": m.audio, "video": m.video]
    }

    private func embedAudio(_ r: Request) async throws -> [String: Any] {
        // semd validates requests: a bad one is its bug.
        guard let path = r.path, path.hasPrefix("/"), let start = r.startMS, let end = r.endMS,
              start >= 0, (minAudioWindowMS...maxAudioWindowMS).contains(end - start)
        else { throw Failure.internal("malformed embed_audio request") }
        let samples = try await loadAudio(path, startMS: start, endMS: end)
        // Shorter than the encoder's minimum or quiet: nothing to embed.
        guard samples.count >= 161, rms(samples) >= silenceRMS else { return ["id": r.id, "vectors": [String](), "silent": true] }
        return ["id": r.id, "vectors": [encodeVector(try engine.embed(audio: samples))]]
    }

    private func embedFrames(_ r: Request) async throws -> [String: Any] {
        guard let path = r.path, path.hasPrefix("/"), let times = r.timesMS,
              (1...maxFramesPerCall).contains(times.count), times.allSatisfy({ $0 >= 0 })
        else { throw Failure.internal("malformed embed_frames request") }
        let frames = try await loadFrames(path, timesMS: times)
        return ["id": r.id, "vectors": try frames.map { encodeVector(try engine.embed(frame: $0)) }]
    }
    /// A page for the quick view, the match highlighted. It needs no
    /// weights: semd offers it with the "pdf" modality.
    private func renderPDFPage(_ r: Request) throws -> [String: Any] {
        // semd validates requests: a bad one is its bug.
        let start = r.matchStart ?? 0, end = r.matchEnd ?? 0
        guard let path = r.path, path.hasPrefix("/"), let n = r.from, n >= 1, start >= 0, end >= start,
              end <= maxPDFPageText
        else { throw Failure.internal("malformed render_pdf_page request") }
        let doc = try pdfs.open(path)
        guard n <= doc.pageCount else { throw Failure.invalid("no page \(n) of \(doc.pageCount)") }
        guard let page = doc.page(at: n - 1) else { throw Failure.invalid("cannot read page \(n)") }
        var highlight: PDFSelection?
        if let text = page.string, let range = utf16Range(of: text, start: start, end: end) {
            highlight = page.selection(for: range)
        }
        let png = try renderPagePNG(page, highlight: highlight)
        return ["id": r.id, "page_count": doc.pageCount, "png": png.base64EncodedString()]
    }

    private func extractPDF(_ r: Request) throws -> [String: Any] {
        // semd validates requests: a bad one is its bug.
        guard let path = r.path, path.hasPrefix("/"), let from = r.from, let count = r.count,
              from >= 1, (1...maxPDFPagesPerCall).contains(count), (0...count).contains(r.render ?? 0)
        else { throw Failure.internal("malformed extract_pdf request") }
        let doc = try pdfs.open(path)
        let n = doc.pageCount
        var budget = r.render ?? 0
        var pages: [[String: Any]] = []
        var i = from
        while i <= n && i < from + count {
            guard let page = doc.page(at: i - 1) else { throw Failure.invalid("cannot read page \(i)") }
            let text = page.string ?? ""
            guard text.utf8.count <= maxPDFPageText else {
                throw Failure.invalid("page \(i) has \(text.utf8.count) bytes of text, cap \(maxPDFPageText)")
            }
            var p: [String: Any] = ["page": i, "text": text]
            if tooLittleText(text) {
                if budget > 0 {
                    budget -= 1
                    guard let ref = page.pageRef else { throw Failure.invalid("cannot render page \(i)") }
                    p["vector"] = encodeVector(try engine.embed(image: try renderPage(ref)))
                } else {
                    p["unrendered"] = true
                }
            }
            pages.append(p)
            i += 1
        }
        return ["id": r.id, "page_count": n, "pages": pages]
    }
}

/// Makes cache hold LiteRT's compiled programs for the model file at path
/// only. LiteRT names them after the file's size and mtime and never deletes
/// any, so a model downloaded again would leave hundreds of MB behind: they
/// are deleted whenever the file they were made for changes. It is kept
/// out of backups, being rebuilt on demand.
func prepareCache(_ cache: URL, for path: String) throws {
    let fm = FileManager.default
    let a = try fm.attributesOfItem(atPath: path)
    guard let size = (a[.size] as? NSNumber)?.uint64Value, let mtime = a[.modificationDate] as? Date,
          let inode = (a[.systemFileNumber] as? NSNumber)?.uint64Value
    else { throw Failure.internal("stat \(path): missing attributes") }
    let key = "\(modelSHA256) \(size) \(inode) \(mtime.timeIntervalSince1970)\n"
    let stamp = cache.appendingPathComponent("MODEL")
    if (try? String(contentsOf: stamp, encoding: .utf8)) != key {
        if fm.fileExists(atPath: cache.path) {
            // LiteRT's files only: they are named after the model file.
            for name in try fm.contentsOfDirectory(atPath: cache.path) where name.hasPrefix(modelFile) {
                try fm.removeItem(at: cache.appendingPathComponent(name))
            }
            log("cache \(cache.path) cleared: made for another model file")
        }
        try fm.createDirectory(at: cache, withIntermediateDirectories: true)
        try key.write(to: stamp, atomically: true, encoding: .utf8)
    }
    var dir = cache
    var v = URLResourceValues()
    v.isExcludedFromBackup = true
    try dir.setResourceValues(v)
}

@main
struct SemdEmbed {
    static func main() async {
        let opt = parseOptions()
        let file = opt.model.appendingPathComponent(modelPart).appendingPathComponent(modelFile).path
        // internal/model verified the file when installing it; a wrong size
        // means it was changed since, and would embed into another space.
        let size: UInt64
        do {
            size = (try FileManager.default.attributesOfItem(atPath: file)[.size] as? NSNumber)?.uint64Value ?? 0
        } catch {
            die("no model: \(error.localizedDescription)")
        }
        guard size == modelSize else { die("\(file) is \(size) bytes, not \(modelSize): not the pinned model") }
        if let cache = opt.cache {
            do {
                try prepareCache(cache, for: file)
            } catch {
                die("cache \(cache.path): \(error.localizedDescription)")
            }
        }
        let t0 = Date()
        let engine: Engine
        do {
            engine = try Engine(model: file, cache: opt.cache?.path, dims: opt.dims)
        } catch {
            die("load model: \(error)")
        }
        log(String(format: "model %@ (%@) loaded in %.2fs", modelID, modelPrecision, Date().timeIntervalSince(t0)))

        let out = Output()
        let jobs = Jobs()
        let w = Worker(engine: engine, pdfs: PDFCache(), out: out)

        // Worker: one request at a time, at the QoS its priority asks for.
        let worker = Task {
            while let r = await jobs.next() {
                let prio: TaskPriority = r.priority == "query" ? .userInitiated : .utility
                await Task(priority: prio) { await w.run(r) }.value
            }
        }

        // Idle shutdown.
        if opt.idleExit > 0 {
            Task.detached(priority: .background) {
                while true {
                    try? await Task.sleep(for: .seconds(15))
                    if await jobs.idleFor() >= opt.idleExit {
                        log("idle for \(Int(opt.idleExit))s; exiting")
                        exit(0)
                    }
                }
            }
        }

        do {
            for try await line in RequestLines(bytes: FileHandle.standardInput.bytes) {
                guard let r = try? JSONDecoder().decode(Request.self, from: line) else {
                    die("malformed request line")  // semd never sends one: fail loudly
                }
                switch r.op {
                case "hello":
                    guard r.protocolVersion == protocolVersion else {
                        out.fail(r.id, "internal", "protocol \(r.protocolVersion ?? 0) unsupported, want \(protocolVersion)")
                        continue
                    }
                    out.send([
                        "id": r.id, "protocol": protocolVersion, "model": modelID, "precision": modelPrecision,
                        "dims": opt.dims, "modalities": ["text", "image", "pdf", "video", "audio"],
                        "max_batch": maxBatch, "max_tokens": maxTokens,
                    ])
                case "embed_text", "embed_image", "extract_pdf", "render_pdf_page", "probe_media", "embed_audio", "embed_frames":
                    await jobs.push(r)
                default:
                    out.fail(r.id, "internal", "unknown op \(r.op)")
                }
            }
        } catch {
            die("read stdin: \(error)")
        }
        // stdin EOF: finish queued work, then exit.
        await jobs.close()
        await worker.value
        exit(0)
    }
}

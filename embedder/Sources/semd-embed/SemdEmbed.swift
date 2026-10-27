// semd-embed: stateless embedding sidecar for semd.
//
// Speaks the NDJSON protocol specified in internal/embed/sidecar.go on
// stdin/stdout; logs go to stderr. One model, one serial worker: queries are
// taken before queued index work. The vision and audio towers load on first
// use and unload after towerIdle. Exits on stdin EOF, and with status 0
// after an idle period (semd restarts it on demand).

import EmbeddingGemma2
import Foundation
import MLX

let protocolVersion = 1
let maxBatch = 32
let maxTokens = 8192
let towerIdle: TimeInterval = 120
/// Internal op the idle timer queues so the worker itself unloads a tower
/// (named in path). Never accepted from stdin.
let unloadOp = "_unload"

func log(_ s: String) {
    FileHandle.standardError.write(Data((s + "\n").utf8))
}

func die(_ s: String) -> Never {
    log("fatal: " + s)
    exit(1)
}

// MARK: options

struct Options {
    var model: URL
    var dims: Int
    var idleExit: TimeInterval
}

func parseOptions() -> Options {
    var model: String?
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
        case "--dims":
            guard let d = Int(value(a)), [128, 256, 512, 768].contains(d) else { die("--dims must be 128, 256, 512 or 768") }
            dims = d
        case "--idle-exit":
            guard let s = TimeInterval(value(a)), s >= 0 else { die("--idle-exit must be seconds ≥ 0") }
            idle = s
        default: die("unknown argument \(a)")
        }
    }
    guard let model else { die("usage: semd-embed --model DIR [--dims 256] [--idle-exit SECONDS]") }
    return Options(model: URL(fileURLWithPath: model, isDirectory: true), dims: dims, idleExit: idle)
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
    enum CodingKeys: String, CodingKey {
        case id, op, priority, texts, path, from, count, render
        case protocolVersion = "protocol"
        case startMS = "start_ms", endMS = "end_ms", timesMS = "times_ms"
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
    private var towerUsed: [String: Date] = [:]

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

    /// Queues internal work without counting as activity.
    func pushInternal(_ r: Request) {
        if let w = waiter {
            waiter = nil
            busy = true
            w.resume(returning: r)
        } else {
            index.append(r)
        }
    }

    func used(_ tower: String) { towerUsed[tower] = Date() }

    /// The towers idle for t, each reported once per idle period.
    func idleTowers(_ t: TimeInterval) -> [String] {
        guard !busy else { return [] }
        let now = Date()
        let idle = towerUsed.filter { now.timeIntervalSince($0.value) >= t }.map(\.key).sorted()
        for name in idle { towerUsed[name] = nil }
        return idle
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
func modelID(text: URL, vision: URL?, audio: URL?) -> String {
    func sha(_ dir: URL) -> Substring {
        // Quantized weights carry the hash of the converted file; unconverted
        // ones (audio-bf16) only that of the source.
        struct Manifest: Decodable {
            var output_sha256: String? = nil
            var sha256: String? = nil
        }
        guard let data = try? Data(contentsOf: dir.appendingPathComponent("manifest.json")),
              let m = try? JSONDecoder().decode(Manifest.self, from: data),
              let h = m.output_sha256 ?? m.sha256, h.count >= 12
        else { die("cannot read \(dir.path)/manifest.json") }
        return h.prefix(12)
    }
    var id = "embeddinggemma-2/" + sha(text)
    if let vision { id += "+v" + sha(vision) }
    if let audio { id += "+a" + sha(audio) }
    return id
}

/// Everything the worker needs; touched by the worker only.
struct Worker: Sendable {
    let encoder: TextEncoder
    let dims: Int
    let vision: Tower?
    let audio: Tower?
    let pdfs: PDFCache
    let jobs: Jobs
    let out: Output

    func run(_ r: Request) async {
        if r.op == unloadOp {
            switch r.path {
            case vision?.name:
                await vision?.unload()
                pdfs.drop()
            case audio?.name:
                await audio?.unload()
            default:
                die("unload of unknown tower \(r.path ?? "")")
            }
            return
        }
        do {
            out.send(try await handle(r))
        } catch let f as Failure {
            out.fail(r.id, f.code, f.message)
        } catch let e as EmbeddingError {
            out.fail(r.id, "invalid", e.description)
        } catch {
            out.fail(r.id, "internal", String(describing: error))
        }
    }

    private func handle(_ r: Request) async throws -> [String: Any] {
        switch r.op {
        case "embed_text": return try await embedText(r)
        case "embed_image": return try await embedImage(r)
        case "extract_pdf": return try await extractPDF(r)
        case "probe_media": return try await probeMedia(r)
        case "embed_audio": return try await embedAudio(r)
        case "embed_frames": return try await embedFrames(r)
        default: throw Failure.internal("unknown op \(r.op)")
        }
    }

    private func embedText(_ r: Request) async throws -> [String: Any] {
        guard let texts = r.texts, !texts.isEmpty else { throw Failure.invalid("no texts") }
        guard texts.count <= maxBatch else { throw Failure.invalid("batch of \(texts.count) exceeds \(maxBatch)") }
        // Prompts were applied by semd: encode as-is.
        let vectors = try await encoder.encode(texts, task: .raw, dimensions: dims)
        guard vectors.count == texts.count, vectors.allSatisfy({ $0.count == dims }) else {
            throw Failure.internal("encoder returned the wrong shape")
        }
        return ["id": r.id, "vectors": vectors.map(encodeVector)]
    }

    private func embedImageVector(_ image: RGBImage, softTokens: Int = imageSoftTokens) async throws -> String {
        guard let vision else { throw Failure.internal("no vision weights") }
        let prepared = try ImageProcessor.prepare(image, maxSoftTokens: softTokens)
        await vision.ensure()
        let v = try await encoder.encodeImage(prepared, dimensions: dims)
        await jobs.used(vision.name)
        guard v.count == dims else { throw Failure.internal("image vector has \(v.count) dims") }
        return encodeVector(v)
    }

    private func embedImage(_ r: Request) async throws -> [String: Any] {
        guard let path = r.path, path.hasPrefix("/") else { throw Failure.internal("embed_image needs an absolute path") }
        return ["id": r.id, "vectors": [try await embedImageVector(try loadImage(path))]]
    }

    private func probeMedia(_ r: Request) async throws -> [String: Any] {
        guard let path = r.path, path.hasPrefix("/") else { throw Failure.internal("probe_media needs an absolute path") }
        let m = try await probeAsset(path)
        return ["id": r.id, "duration_ms": m.durationMS, "audio": m.audio, "video": m.video]
    }

    private func embedAudio(_ r: Request) async throws -> [String: Any] {
        // semd validates requests: a bad one is its bug.
        guard let audio, let path = r.path, path.hasPrefix("/"), let start = r.startMS, let end = r.endMS,
              start >= 0, (minAudioWindowMS...maxAudioWindowMS).contains(end - start)
        else { throw Failure.internal("malformed embed_audio request") }
        let samples = try await loadAudio(path, startMS: start, endMS: end)
        // Shorter than the encoder's minimum or quiet: nothing to embed.
        guard samples.count >= 161, rms(samples) >= silenceRMS else { return ["id": r.id, "vectors": [String](), "silent": true] }
        let prepared = try AudioProcessor.prepare(samples: samples)
        await audio.ensure()
        let v = try await encoder.encodeAudio(prepared, dimensions: dims)
        await jobs.used(audio.name)
        guard v.count == dims else { throw Failure.internal("audio vector has \(v.count) dims") }
        return ["id": r.id, "vectors": [encodeVector(v)]]
    }

    private func embedFrames(_ r: Request) async throws -> [String: Any] {
        guard let path = r.path, path.hasPrefix("/"), let times = r.timesMS,
              (1...maxFramesPerCall).contains(times.count), times.allSatisfy({ $0 >= 0 })
        else { throw Failure.internal("malformed embed_frames request") }
        var vectors: [String] = []
        for img in try await loadFrames(path, timesMS: times) {
            vectors.append(try await embedImageVector(img, softTokens: frameSoftTokens))
        }
        return ["id": r.id, "vectors": vectors]
    }

    private func extractPDF(_ r: Request) async throws -> [String: Any] {
        // semd validates requests: a bad one is its bug.
        guard let path = r.path, path.hasPrefix("/"), let from = r.from, let count = r.count,
              from >= 1, (1...maxPDFPagesPerCall).contains(count), (0...count).contains(r.render ?? 0)
        else { throw Failure.internal("malformed extract_pdf request") }
        guard vision != nil else { throw Failure.internal("no vision weights") }
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
                    p["vector"] = try await embedImageVector(try renderPage(ref))
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

@main
struct SemdEmbed {
    static func main() async {
        let opt = parseOptions()
        // Bound reusable GPU buffers so the idle footprint stays small.
        Memory.cacheLimit = 64 * 1024 * 1024
        let textDir = opt.model.appendingPathComponent("text-q8", isDirectory: true)
        var visionDir: URL? = opt.model.appendingPathComponent("vision-q8", isDirectory: true)
        if let d = visionDir, !FileManager.default.fileExists(atPath: d.appendingPathComponent("manifest.json").path) {
            log("warning: no \(d.path): images and PDFs are not offered")
            visionDir = nil
        }
        var audioDir: URL? = opt.model.appendingPathComponent("audio-bf16", isDirectory: true)
        if let d = audioDir, !FileManager.default.fileExists(atPath: d.appendingPathComponent("manifest.json").path) {
            log("warning: no \(d.path): audio is not offered")
            audioDir = nil
        }
        let id = modelID(text: textDir, vision: visionDir, audio: audioDir)
        var modalities = ["text"]
        if visionDir != nil { modalities += ["image", "pdf", "video"] }
        if audioDir != nil { modalities += ["audio"] }
        let t0 = Date()
        let encoder: TextEncoder
        do {
            encoder = try await TextEncoder(directory: textDir, progress: { log("load: " + $0) })
        } catch {
            die("load model: \(error)")
        }
        let precision = await encoder.weightFormat()
        log(String(format: "model %@ (%@) loaded in %.2fs", id, precision, Date().timeIntervalSince(t0)))

        let out = Output()
        let jobs = Jobs()
        let w = Worker(
            encoder: encoder, dims: opt.dims,
            vision: visionDir.map { dir in
                Tower(name: "vision", load: { try await encoder.enableVision(directory: dir) }, drop: { await encoder.disableVision() })
            },
            audio: audioDir.map { dir in
                Tower(name: "audio", load: { try await encoder.enableAudio(directory: dir) }, drop: { await encoder.disableAudio() })
            },
            pdfs: PDFCache(), jobs: jobs, out: out)

        // Worker: one request at a time, at the QoS its priority asks for.
        let worker = Task {
            while let r = await jobs.next() {
                let prio: TaskPriority = r.priority == "query" ? .userInitiated : .utility
                await Task(priority: prio) { await w.run(r) }.value
            }
        }

        // Vision (~200 MB) and audio (~600 MB) weights leave memory when unused for a while.
        Task.detached(priority: .background) {
            while true {
                try? await Task.sleep(for: .seconds(15))
                for name in await jobs.idleTowers(towerIdle) {
                    await jobs.pushInternal(Request(id: 0, op: unloadOp, path: name))
                }
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
            for try await line in FileHandle.standardInput.bytes.lines {
                guard let r = try? JSONDecoder().decode(Request.self, from: Data(line.utf8)) else {
                    die("malformed request line")  // semd never sends one: fail loudly
                }
                switch r.op {
                case "hello":
                    guard r.protocolVersion == protocolVersion else {
                        out.fail(r.id, "internal", "protocol \(r.protocolVersion ?? 0) unsupported, want \(protocolVersion)")
                        continue
                    }
                    out.send([
                        "id": r.id, "protocol": protocolVersion, "model": id, "precision": precision,
                        "dims": opt.dims, "modalities": modalities, "max_batch": maxBatch, "max_tokens": maxTokens,
                    ])
                case "embed_text", "embed_image", "extract_pdf", "probe_media", "embed_audio", "embed_frames":
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

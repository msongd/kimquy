// The LiteRT-LM embedding engine: EmbeddingGemma 2 (740M, int4) from one
// .litertlm file holding the text, vision and audio towers. Images reach it
// as PNG, audio as 16 kHz mono WAV: formats it surely reads.

import CLiteRTLM
import CoreGraphics
import Foundation
import ImageIO
import UniformTypeIdentifiers

/// The model file: its folder in a model directory (internal/model's part),
/// its name, and the size and sha256 internal/model pins. semd-embed checks
/// the size only; the installer verified the hash.
let modelPart = "litert"
let modelFile = "embeddinggemma-2-740m.litertlm"
let modelSize: UInt64 = 484_622_336
let modelSHA256 = "e7a8a2204b91e0f96e92960e84a09a89212e1633dcb7575a9bf3378b4df77f4c"
let modelPrecision = "litert-int4"

/// Throws on a non-OK status, with LiteRT's message for it. Statuses that
/// blame the input are "invalid"; any other is a sidecar fault.
func check(_ st: LiteRtLmStatusCode, _ what: String) throws {
    guard st != kLiteRtLmStatusOk else { return }
    let msg = litert_lm_get_last_error_message().map { String(cString: $0) } ?? "no message"
    litert_lm_clear_last_error()
    let text = "\(what): status \(st.rawValue): \(msg)"
    switch st {
    case kLiteRtLmStatusInvalidArgument, kLiteRtLmStatusOutOfRange: throw Failure.invalid(text)
    default: throw Failure.internal(text)
    }
}

/// Used by the worker only: LiteRT engines are not thread-safe.
final class Engine: @unchecked Sendable {
    let dims: Int
    private let engine: OpaquePointer
    private let options: OpaquePointer  // text, images, audio
    private let frameOptions: OpaquePointer

    /// Loads all towers on the GPU. cache, when not nil, keeps compiled GPU
    /// programs across runs, which cuts a cold start from seconds to one.
    init(model: String, cache: String?, dims: Int) throws {
        self.dims = dims
        _ = litert_lm_set_min_log_level(kLiteRtLmLogSeverityWarning)
        var s: OpaquePointer?
        try check(litert_lm_embedding_engine_settings_create(model, "gpu", "gpu", "gpu", &s), "engine settings")
        guard let settings = s else { throw Failure.internal("engine settings: NULL") }
        defer { litert_lm_embedding_engine_settings_delete(settings) }
        if let cache {
            try check(litert_lm_embedding_engine_settings_set_cache_dir(settings, cache), "cache dir")
        }
        try check(litert_lm_embedding_engine_settings_set_max_input_length(settings, Int32(maxTokens)), "max input")
        try check(litert_lm_embedding_engine_settings_set_vision_tokens_per_image(settings, Int32(imageSoftTokens)), "vision tokens")
        var e: OpaquePointer?
        try check(litert_lm_embedding_engine_create(settings, &e), "engine")
        guard let e else { throw Failure.internal("engine: NULL") }
        engine = e
        func makeOptions(_ tokens: Int) throws -> OpaquePointer {
            var o: OpaquePointer?
            try check(litert_lm_embedding_options_create(&o), "options")
            guard let o else { throw Failure.internal("options: NULL") }
            try check(litert_lm_embedding_options_set_output_size(o, Int32(dims)), "output size")
            // Too long an input is the caller's error, not silently cut.
            try check(litert_lm_embedding_options_set_input_overflow_strategy(o, kLiteRtLmInputOverflowStrategyError), "overflow")
            try check(litert_lm_embedding_options_set_vision_tokens_per_image(o, Int32(tokens)), "vision tokens")
            return o
        }
        options = try makeOptions(imageSoftTokens)
        frameOptions = try makeOptions(frameSoftTokens)
    }

    private func vector(_ resp: OpaquePointer) throws -> [Float] {
        var n = 0
        try check(litert_lm_embedding_response_get_size(resp, &n), "response size")
        var p: UnsafePointer<Float>?
        try check(litert_lm_embedding_response_get_values(resp, &p), "response values")
        guard n == dims, let p else { throw Failure.internal("vector has \(n) dims, want \(dims)") }
        let v = Array(UnsafeBufferPointer(start: p, count: n))
        guard v.allSatisfy(\.isFinite) else { throw Failure.internal("non-finite vector") }
        return v
    }

    private func input(_ type: LiteRtLmInputDataType, _ data: Data) throws -> OpaquePointer {
        var out: OpaquePointer?
        try data.withUnsafeBytes { try check(litert_lm_input_data_create(type, $0.baseAddress, data.count, &out), "input") }
        guard let out else { throw Failure.internal("input: NULL") }
        return out
    }

    /// Embeds texts as given (semd applied the prompts), one batch.
    func embed(texts: [String]) throws -> [[Float]] {
        let ins = try texts.map { try input(kLiteRtLmInputDataTypeText, Data($0.utf8)) }
        defer { ins.forEach { litert_lm_input_data_delete($0) } }
        // A batch is rows of inputs; each row here is one text.
        let cells = UnsafeMutablePointer<OpaquePointer?>.allocate(capacity: ins.count)
        defer { cells.deallocate() }
        cells.initialize(from: ins.map { Optional($0) }, count: ins.count)
        var rows: [UnsafePointer<OpaquePointer?>?] = (0..<ins.count).map { UnsafePointer(cells + $0) }
        var counts = [Int](repeating: 1, count: ins.count)
        var resps: OpaquePointer?
        try rows.withUnsafeMutableBufferPointer { r in
            try counts.withUnsafeMutableBufferPointer { c in
                try check(litert_lm_embedding_engine_compute_embedding_batch(
                    engine, r.baseAddress, c.baseAddress, ins.count, options, &resps), "embed texts")
            }
        }
        guard let resps else { throw Failure.internal("embed texts: NULL") }
        defer { litert_lm_embedding_responses_delete(resps) }
        var n = 0
        try check(litert_lm_embedding_responses_get_size(resps, &n), "responses size")
        guard n == texts.count else { throw Failure.internal("\(n) vectors for \(texts.count) texts") }
        return try (0..<n).map { i in
            var r: OpaquePointer?
            try check(litert_lm_embedding_responses_get_at(resps, i, &r), "response \(i)")
            guard let r else { throw Failure.internal("response \(i): NULL") }
            return try vector(r)
        }
    }

    private func embedOne(_ type: LiteRtLmInputDataType, _ data: Data, _ opts: OpaquePointer, _ what: String) throws -> [Float] {
        let i = try input(type, data)
        defer { litert_lm_input_data_delete(i) }
        var ins: [OpaquePointer?] = [i]
        var resp: OpaquePointer?
        try ins.withUnsafeMutableBufferPointer { b in
            try check(litert_lm_embedding_engine_compute_embedding(engine, b.baseAddress, 1, opts, &resp), what)
        }
        guard let resp else { throw Failure.internal("\(what): NULL") }
        defer { litert_lm_embedding_response_delete(resp) }
        return try vector(resp)
    }

    /// An image (a photo or a PDF page) at imageSoftTokens.
    func embed(image: CGImage) throws -> [Float] {
        try embedOne(kLiteRtLmInputDataTypeImage, try png(image), options, "embed image")
    }

    /// A video frame at frameSoftTokens.
    func embed(frame: CGImage) throws -> [Float] {
        try embedOne(kLiteRtLmInputDataTypeImage, try png(frame), frameOptions, "embed frame")
    }

    /// 16 kHz mono samples in [-1, 1].
    func embed(audio samples: [Float]) throws -> [Float] {
        try embedOne(kLiteRtLmInputDataTypeAudio, wav(samples), options, "embed audio")
    }
}

/// img as PNG.
func png(_ img: CGImage) throws -> Data {
    let out = NSMutableData()
    guard let dst = CGImageDestinationCreateWithData(out, UTType.png.identifier as CFString, 1, nil) else {
        throw Failure.internal("no PNG encoder")
    }
    CGImageDestinationAddImage(dst, img, nil)
    guard CGImageDestinationFinalize(dst) else { throw Failure.internal("cannot encode PNG") }
    return out as Data
}

/// 16 kHz mono 16-bit PCM WAV of samples in [-1, 1].
func wav(_ samples: [Float]) -> Data {
    var d = Data(capacity: 44 + samples.count * 2)
    func u32(_ x: UInt32) { withUnsafeBytes(of: x.littleEndian) { d.append(contentsOf: $0) } }
    func u16(_ x: UInt16) { withUnsafeBytes(of: x.littleEndian) { d.append(contentsOf: $0) } }
    let bytes = UInt32(samples.count * 2)
    d.append(contentsOf: Array("RIFF".utf8))
    u32(36 + bytes)
    d.append(contentsOf: Array("WAVEfmt ".utf8))
    u32(16)
    u16(1)  // PCM
    u16(1)  // mono
    u32(UInt32(audioRate))
    u32(UInt32(audioRate * 2))
    u16(2)
    u16(16)
    d.append(contentsOf: Array("data".utf8))
    u32(bytes)
    for x in samples {
        u16(UInt16(bitPattern: Int16((max(-1, min(1, x)) * 32767).rounded())))
    }
    return d
}

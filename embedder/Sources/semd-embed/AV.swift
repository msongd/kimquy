// Audio and video decoding for semd-embed via AVFoundation. semd plans the
// windows and frame times; each call here stays small so queries never
// wait long behind a long recording. The limits mirror internal/embed/av.go.

import AVFoundation
import CoreGraphics
import EmbeddingGemma2
import Foundation

let maxAudioWindowMS = 30_000
let minAudioWindowMS = 100
let maxFramesPerCall = 8
let audioRate = 16_000
/// A window quieter than this RMS (about -60 dBFS) is reported as silent
/// rather than embedded: silence would match every vague query.
let silenceRMS: Float = 0.001
/// Frames are decoded at most this large; the 70-token budget needs ~400 px.
let frameDecodeMaxSide = 512
/// A frame may come from this far around the requested time, so decoding
/// can start at a nearby keyframe instead of decoding a whole GOP.
let frameToleranceMS = 1000

struct MediaInfo {
    let durationMS: Int
    let audio: Bool
    let video: Bool
}

/// Opens an asset and checks that it is local, readable and unprotected.
func openAsset(_ path: String) async throws -> AVURLAsset {
    guard FileManager.default.isReadableFile(atPath: path) else { throw Failure.invalid("file is not readable") }
    let asset = AVURLAsset(url: URL(fileURLWithPath: path), options: [AVURLAssetPreferPreciseDurationAndTimingKey: true])
    let readable: Bool, protected: Bool
    do {
        (readable, protected) = try await asset.load(.isReadable, .hasProtectedContent)
    } catch {
        throw Failure.invalid("not a readable media file: \(error.localizedDescription)")
    }
    guard readable else { throw Failure.invalid("not a readable media file") }
    guard !protected else { throw Failure.invalid("media is DRM-protected") }
    return asset
}

func probeAsset(_ path: String) async throws -> MediaInfo {
    let asset = try await openAsset(path)
    let duration: CMTime, audio: [AVAssetTrack], video: [AVAssetTrack]
    do {
        duration = try await asset.load(.duration)
        audio = try await asset.loadTracks(withMediaType: .audio)
        video = try await asset.loadTracks(withMediaType: .video)
    } catch {
        throw Failure.invalid("cannot read media tracks: \(error.localizedDescription)")
    }
    guard duration.isNumeric, duration.seconds.isFinite, duration.seconds > 0 else {
        throw Failure.invalid("media has no duration")
    }
    guard duration.seconds < 7 * 24 * 3600 else { throw Failure.invalid("media lasts \(Int(duration.seconds))s, over a week") }
    guard !audio.isEmpty || !video.isEmpty else { throw Failure.invalid("media has no audio or video track") }
    return MediaInfo(durationMS: max(1, Int((duration.seconds * 1000).rounded(.down))), audio: !audio.isEmpty, video: !video.isEmpty)
}

/// Decodes [startMS, endMS) of all audio tracks, mixed down to 16 kHz mono
/// float in [-1, 1]. A window past the end of the audio comes back short.
func loadAudio(_ path: String, startMS: Int, endMS: Int) async throws -> [Float] {
    let asset = try await openAsset(path)
    let tracks: [AVAssetTrack]
    do {
        tracks = try await asset.loadTracks(withMediaType: .audio)
    } catch {
        throw Failure.invalid("cannot read audio tracks: \(error.localizedDescription)")
    }
    guard !tracks.isEmpty else { throw Failure.invalid("media has no audio track") }
    let reader: AVAssetReader
    do {
        reader = try AVAssetReader(asset: asset)
    } catch {
        throw Failure.invalid("cannot read audio: \(error.localizedDescription)")
    }
    reader.timeRange = CMTimeRange(
        start: CMTime(value: CMTimeValue(startMS), timescale: 1000),
        duration: CMTime(value: CMTimeValue(endMS - startMS), timescale: 1000))
    let out = AVAssetReaderAudioMixOutput(audioTracks: tracks, audioSettings: [
        AVFormatIDKey: kAudioFormatLinearPCM,
        AVSampleRateKey: audioRate,
        AVNumberOfChannelsKey: 1,
        AVLinearPCMBitDepthKey: 32,
        AVLinearPCMIsFloatKey: true,
        AVLinearPCMIsBigEndianKey: false,
        AVLinearPCMIsNonInterleaved: false,
    ])
    out.alwaysCopiesSampleData = false
    guard reader.canAdd(out) else { throw Failure.invalid("cannot decode this audio to PCM") }
    reader.add(out)
    guard reader.startReading() else {
        throw Failure.invalid("cannot read audio: \(reader.error?.localizedDescription ?? "unknown error")")
    }
    // The window's samples, plus slack for codec frame boundaries.
    let want = (endMS - startMS) * audioRate / 1000
    var samples: [Float] = []
    samples.reserveCapacity(want)
    while let buf = out.copyNextSampleBuffer() {
        guard let block = CMSampleBufferGetDataBuffer(buf) else { continue }
        let n = CMBlockBufferGetDataLength(block)
        guard n % 4 == 0 else { throw Failure.internal("PCM buffer of \(n) bytes is not float32") }
        var chunk = [Float](repeating: 0, count: n / 4)
        let status = chunk.withUnsafeMutableBytes { CMBlockBufferCopyDataBytes(block, atOffset: 0, dataLength: n, destination: $0.baseAddress!) }
        guard status == kCMBlockBufferNoErr else { throw Failure.invalid("cannot copy audio samples (\(status))") }
        samples.append(contentsOf: chunk.prefix(want - samples.count))
        if samples.count >= want {
            reader.cancelReading()
            break
        }
    }
    if reader.status == .failed {
        throw Failure.invalid("audio decoding failed: \(reader.error?.localizedDescription ?? "unknown error")")
    }
    for i in samples.indices {
        let x = samples[i]
        guard x.isFinite else { throw Failure.invalid("decoded audio has non-finite samples") }
        samples[i] = min(1, max(-1, x))  // float decoders may overshoot slightly
    }
    return samples
}

func rms(_ s: [Float]) -> Float {
    guard !s.isEmpty else { return 0 }
    var sum: Double = 0
    for x in s { sum += Double(x) * Double(x) }
    return Float((sum / Double(s.count)).squareRoot())
}

/// Grabs the video frames nearest the given times, display-oriented.
func loadFrames(_ path: String, timesMS: [Int]) async throws -> [RGBImage] {
    let asset = try await openAsset(path)
    let tracks: [AVAssetTrack]
    do {
        tracks = try await asset.loadTracks(withMediaType: .video)
    } catch {
        throw Failure.invalid("cannot read video tracks: \(error.localizedDescription)")
    }
    guard !tracks.isEmpty else { throw Failure.invalid("media has no video track") }
    let gen = AVAssetImageGenerator(asset: asset)
    gen.appliesPreferredTrackTransform = true
    gen.maximumSize = CGSize(width: frameDecodeMaxSide, height: frameDecodeMaxSide)
    let tol = CMTime(value: CMTimeValue(frameToleranceMS), timescale: 1000)
    gen.requestedTimeToleranceBefore = tol
    gen.requestedTimeToleranceAfter = tol
    var images: [RGBImage] = []
    for t in timesMS {
        let img: CGImage
        do {
            img = try await gen.image(at: CMTime(value: CMTimeValue(t), timescale: 1000)).image
        } catch {
            throw Failure.invalid("cannot decode the frame at \(t) ms: \(error.localizedDescription)")
        }
        let rect = CGRect(x: 0, y: 0, width: img.width, height: img.height)
        images.append(try bitmap(width: img.width, height: img.height) { $0.draw(img, in: rect) })
    }
    return images
}

import XCTest
import EmbeddingGemma2

final class AudioTests: XCTestCase {
    func testSilencePaddingAndLogFloor() throws {
        let p=try AudioProcessor.prepare(samples:Array(repeating:0,count:1600))
        XCTAssertEqual(p.frames,10);XCTAssertEqual(p.softTokenCount,3)
        XCTAssertEqual(p.mask,Array(repeating:1,count:9)+[0])
        XCTAssertTrue(p.features.prefix(9*128).allSatisfy { abs($0-log(Float(0.001)))<1e-6 })
        XCTAssertTrue(p.features.suffix(128).allSatisfy { $0==0 })
    }
    func testMinimumClipAndInvalidSamples() throws {
        let p=try AudioProcessor.prepare(samples:Array(repeating:0,count:161))
        XCTAssertEqual(p.frames,1);XCTAssertEqual(p.softTokenCount,1)
        for n in [0,160,480001] { XCTAssertThrowsError(try AudioProcessor.prepare(samples:Array(repeating:0,count:n))) }
        for v in [Float.nan,Float.infinity,Float(1.01)] { XCTAssertThrowsError(try AudioProcessor.prepare(samples:Array(repeating:v,count:161))) }
        XCTAssertThrowsError(try AudioProcessor.prepare(samples:Array(repeating:0,count:161),sampleRate:48000))
    }
    func testWAVRejectsTruncationAndStereo() throws {
        let url=FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString+".wav")
        defer { try? FileManager.default.removeItem(at:url) }
        var bytes=Array("RIFF".utf8)+[UInt8](repeating:0,count:4)+Array("WAVEfmt ".utf8)+[16,0,0,0,1,0,2,0,128,62,0,0,0,0,0,0,4,0,16,0]+Array("data".utf8)+[4,0,0,0,0,0,0,0]
        try Data(bytes).write(to:url);XCTAssertThrowsError(try AudioProcessor.loadWAV(url))
        bytes[22]=1;bytes[32]=2;bytes[40]=255
        try Data(bytes).write(to:url);XCTAssertThrowsError(try AudioProcessor.loadWAV(url))
    }
}

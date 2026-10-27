import XCTest
import Foundation
import MLX
import EmbeddingGemma2

final class QuantizationTests: XCTestCase {
    func testNewDestinationAndSourcePreservation() throws {
        let root=FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at:root,withIntermediateDirectories:true)
        defer { try? FileManager.default.removeItem(at:root) }
        let source=root.appendingPathComponent("source"),output=root.appendingPathComponent("q8")
        try FileManager.default.createDirectory(at:source,withIntermediateDirectories:true)
        let matrix=MLXArray((0..<2048).map { Float($0%71)/71 }).reshaped(32,64).asType(.bfloat16)
        try MLX.save(arrays:["language_model.test.weight":matrix,"language_model.norm.weight":MLXArray([Float(1),2]).asType(.bfloat16)],url:source.appendingPathComponent("model.safetensors"))
        try Data("{}".utf8).write(to:source.appendingPathComponent("config.json"))
        let before=try Data(contentsOf:source.appendingPathComponent("model.safetensors"))
        try QuantizedCheckpoint.convert(source:source,destination:output,bits:8)
        XCTAssertEqual(try Data(contentsOf:source.appendingPathComponent("model.safetensors")),before)
        let packed=try MLX.loadArrays(url:output.appendingPathComponent("model.safetensors"))
        XCTAssertEqual(packed["language_model.test.weight"]!.dtype,.uint32)
        XCTAssertEqual(packed["language_model.test.weight"]!.shape,[32,16])
        XCTAssertEqual(packed["language_model.norm.weight"]!.asArray(Float.self),[1,2])
        XCTAssertThrowsError(try QuantizedCheckpoint.convert(source:source,destination:output,bits:8))
        XCTAssertThrowsError(try QuantizedCheckpoint.convert(source:output,destination:root.appendingPathComponent("repacked"),bits:4))
    }
    func testUnsupportedBitsRejectedBeforeWriting() throws {
        let root=FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        XCTAssertThrowsError(try QuantizedCheckpoint.convert(source:root,destination:root,bits:3))
        XCTAssertFalse(FileManager.default.fileExists(atPath:root.path))
    }
}

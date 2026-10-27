import XCTest
import EmbeddingGemma2

final class ImageTests: XCTestCase {
    func testBudgetAndExtremeAspectRatio() throws {
        for budget in [70,140,280] {
            for (w,h) in [(1,20000),(20000,1),(83,47),(47,83),(93,93)] {
                let size = try ImageProcessor.targetSize(width:w,height:h,maxSoftTokens:budget)
                XCTAssertEqual(size.width % 48,0); XCTAssertEqual(size.height % 48,0)
                XCTAssertGreaterThan(size.width*size.height,0)
                XCTAssertLessThanOrEqual(size.width*size.height,budget*9*256)
            }
        }
        XCTAssertThrowsError(try ImageProcessor.targetSize(width:1,height:0))
        XCTAssertThrowsError(try ImageProcessor.targetSize(width:100,height:100,maxSoftTokens:71))
    }
    func testIdentityResizeAndRGBBounds() throws {
        let bytes: [UInt8] = [255,0,0,0,255,0,0,0,255,255,255,255]
        let image = try RGBImage(width:2,height:2,bytes:bytes)
        XCTAssertEqual(try ImageProcessor.resize(image,width:2,height:2).bytes,bytes)
        XCTAssertThrowsError(try RGBImage(width:2,height:2,bytes:[0]))
    }
    func testPaddingAndPatchCoordinates() throws {
        let image = try RGBImage(width:1,height:1,bytes:[255,128,0])
        let result = try ImageProcessor.prepare(image,maxSoftTokens:70)
        XCTAssertEqual(result.width,384); XCTAssertEqual(result.height,384)
        XCTAssertEqual(result.patchCount,576); XCTAssertEqual(result.softTokenCount,64)
        XCTAssertEqual(Array(result.pixels.prefix(3)),[1,Float(128.0/255),0])
        XCTAssertEqual(Array(result.positions.prefix(4)),[0,0,1,0])
        XCTAssertTrue(result.pixels.dropFirst(576*768).allSatisfy { $0 == 0 })
        XCTAssertTrue(result.positions.dropFirst(576*2).allSatisfy { $0 == -1 })
    }
}

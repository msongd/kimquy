import XCTest
import MLX
import EmbeddingGemma2

final class MaskTests: XCTestCase {
    func testLocalAttentionIsBidirectionalAndInclusive() {
        let mask = MLXArray([Int32(1),1,1,1,0]).reshaped(1,5)
        let actual = TextModel.attentionMask(mask,length:5,window:1).asArray(Bool.self)
        // Keys at exactly +/- 1 are included; padding keys are excluded even for padding queries.
        XCTAssertEqual(actual, [
            true,true,false,false,false,
            true,true,true,false,false,
            false,true,true,true,false,
            false,false,true,true,false,
            false,false,false,true,false
        ])
    }
    func testFullAttentionMasksPaddingOnly() {
        let actual = TextModel.attentionMask(MLXArray([Int32(1),1,0]).reshaped(1,3),length:3,window:nil)
        XCTAssertEqual(actual.shape,[1,1,1,3])
        XCTAssertEqual(actual.asArray(Bool.self),[true,true,false])
    }
    func testTaskAsymmetryAndUnicodePreservation() {
        XCTAssertEqual(EmbeddingTask.search.format("Москва 😀"),"task: search result | query: Москва 😀")
        XCTAssertEqual(EmbeddingTask.document.format("текст",title:"Заголовок"),"title: Заголовок | text: текст")
        XCTAssertEqual(EmbeddingTask.raw.format("title: none | text: a"),"title: none | text: a")
    }
}

// HelperResultTests.swift
import XCTest
@testable import CitadelKit

final class HelperResultTests: XCTestCase {
    func testEncodeDecodeRoundTrip() {
        let original = HelperResult(ok: true, detail: "brought up 100.64.0.5")
        let decoded = HelperResult.decode(original.encoded())
        XCTAssertEqual(decoded.ok, original.ok)
        XCTAssertEqual(decoded.detail, original.detail)
    }

    func testDecodeOfNilDataFailsSafe() {
        // A helper that returned no data (e.g. crashed mid-call) must read
        // as a failure, never as an unintentional success.
        let decoded = HelperResult.decode(nil)
        XCTAssertFalse(decoded.ok)
    }

    func testDecodeOfGarbageDataFailsSafe() {
        let decoded = HelperResult.decode(Data("not json".utf8))
        XCTAssertFalse(decoded.ok)
    }
}

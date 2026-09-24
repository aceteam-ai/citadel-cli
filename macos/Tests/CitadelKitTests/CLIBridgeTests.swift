// CLIBridgeTests.swift
import XCTest
@testable import CitadelKit

final class CLIBridgeTests: XCTestCase {
    /// Writes a tiny shell script standing in for the real `citadel` binary
    /// so these tests never depend on a built citadel-cli binary being
    /// present on the machine running `swift test`.
    private func makeFakeBinary(script: String) throws -> String {
        let dir = FileManager.default.temporaryDirectory
            .appendingPathComponent("citadelkit-tests-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        let path = dir.appendingPathComponent("citadel").path
        try script.write(toFile: path, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: path)
        return path
    }

    func testFetchStatusDecodesConnectedPeers() throws {
        let json = """
        {"nodeName":"test-node","nodeIP":"100.64.0.5","connected":true,"version":"v1.0.0",
         "peers":[{"hostname":"peer-a","ip":"100.64.0.6","online":true,"latency":"12ms","connType":"direct"}]}
        """
        let script = "#!/bin/sh\ncat <<'EOF'\n\(json)\nEOF\n"
        let binaryPath = try makeFakeBinary(script: script)
        let bridge = CLIBridge(binaryPath: binaryPath)

        let status = try bridge.fetchStatus()
        XCTAssertTrue(status.connected)
        XCTAssertEqual(status.nodeName, "test-node")
        XCTAssertEqual(status.peers.count, 1)
        XCTAssertEqual(status.peers.first?.hostname, "peer-a")
        XCTAssertEqual(status.peers.first?.connType, "direct")
    }

    func testFetchStatusThrowsOnNonZeroExit() throws {
        let script = "#!/bin/sh\necho 'not logged in' >&2\nexit 1\n"
        let binaryPath = try makeFakeBinary(script: script)
        let bridge = CLIBridge(binaryPath: binaryPath)

        XCTAssertThrowsError(try bridge.fetchStatus()) { error in
            guard case CLIBridgeError.processFailed(let code, let stderr) = error else {
                return XCTFail("expected .processFailed, got \(error)")
            }
            XCTAssertEqual(code, 1)
            XCTAssertTrue(stderr.contains("not logged in"))
        }
    }

    func testBinaryNotFoundWhenPathDoesNotExist() {
        let bridge = CLIBridge(binaryPath: "/nonexistent/path/citadel")
        XCTAssertFalse(bridge.isAvailable())
        XCTAssertThrowsError(try bridge.run(["status"])) { error in
            XCTAssertEqual(error as? CLIBridgeError, CLIBridgeError.binaryNotFound)
        }
    }
}

extension CLIBridgeError: Equatable {
    public static func == (lhs: CLIBridgeError, rhs: CLIBridgeError) -> Bool {
        switch (lhs, rhs) {
        case (.binaryNotFound, .binaryNotFound):
            return true
        case (.processFailed(let lc, let ls), .processFailed(let rc, let rs)):
            return lc == rc && ls == rs
        case (.decodeFailed, .decodeFailed):
            return true
        default:
            return false
        }
    }
}

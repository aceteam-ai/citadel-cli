// CLIBridge.swift
// Shells out to the `citadel` binary for status and process-only mode
// connect/disconnect.
//
// Why shell out instead of re-implementing the local API socket protocol in
// Swift: citadel's own status/peer logic already goes through
// internal/network's SelectBackend -> attachedBackend path (see
// internal/network/select.go, backend_attached.go), which is exactly the "GUI
// attaches to a running machine-wide backend rather than starting its own
// WireGuard endpoint" behavior #670 asks for. Invoking the existing `citadel
// status --json` gets that behavior for free and keeps this app from
// carrying a second, divergent implementation of backend selection. The
// tradeoff is documented in the PR description: `citadel status --json`
// today runs a full system-vitals collection (docker stats, GPU query, ...),
// which is heavier than a menu bar app strictly needs for a few-times-a-
// minute poll. A follow-up (either a `--network-only` flag on `citadel
// status`, or a Swift-native reader of the local API socket used by
// attachedBackend) can narrow this without changing the app's structure.
import Foundation

public enum CLIBridgeError: Error, LocalizedError {
    case binaryNotFound
    case processFailed(exitCode: Int32, stderr: String)
    case decodeFailed(Error)

    public var errorDescription: String? {
        switch self {
        case .binaryNotFound:
            return "Could not find the citadel command-line binary."
        case .processFailed(let code, let stderr):
            return "citadel exited with status \(code): \(stderr.trimmingCharacters(in: .whitespacesAndNewlines))"
        case .decodeFailed(let err):
            return "Could not parse citadel's output: \(err.localizedDescription)"
        }
    }
}

/// Locates and runs the `citadel` CLI binary. A class (not a static enum) so
/// tests can inject a fake binary path.
public final class CLIBridge: @unchecked Sendable {
    /// Where to look for the `citadel` binary, in order. The app-bundle
    /// location is where #672's packaging is expected to place it (mirroring
    /// today's packaging/macos/citadel-launcher, which co-locates the CLI
    /// binary at Contents/MacOS/citadel inside Citadel.app) so the GUI and
    /// CLI ship as one artifact; the rest are the well-known install
    /// locations `install.sh` / the Homebrew tap use today.
    public static func candidateBinaryPaths() -> [String] {
        var candidates: [String] = []
        if let bundlePath = Bundle.main.executableURL?.deletingLastPathComponent().appendingPathComponent("citadel").path {
            candidates.append(bundlePath)
        }
        candidates.append(contentsOf: [
            "/opt/homebrew/bin/citadel",
            "/usr/local/bin/citadel",
            NSString(string: "~/.local/bin/citadel").expandingTildeInPath,
        ])
        return candidates
    }

    private let binaryPath: String

    /// - Parameter binaryPath: override the discovered path (used by tests).
    ///   When nil, resolves via `candidateBinaryPaths()` then `PATH`.
    public init(binaryPath: String? = nil) {
        if let binaryPath {
            self.binaryPath = binaryPath
            return
        }
        let fm = FileManager.default
        if let found = Self.candidateBinaryPaths().first(where: { fm.isExecutableFile(atPath: $0) }) {
            self.binaryPath = found
        } else if let onPath = Self.resolveFromPATH("citadel") {
            self.binaryPath = onPath
        } else {
            // Fall back to a bare name; run() will surface .binaryNotFound
            // when /usr/bin/env can't resolve it either, rather than crash
            // at init time.
            self.binaryPath = "citadel"
        }
    }

    private static func resolveFromPATH(_ name: String) -> String? {
        guard let path = ProcessInfo.processInfo.environment["PATH"] else { return nil }
        let fm = FileManager.default
        for dir in path.split(separator: ":") {
            let candidate = "\(dir)/\(name)"
            if fm.isExecutableFile(atPath: candidate) {
                return candidate
            }
        }
        return nil
    }

    public var resolvedBinaryPath: String { binaryPath }

    public func isAvailable() -> Bool {
        FileManager.default.isExecutableFile(atPath: binaryPath)
    }

    /// Runs `citadel <args>` and returns (stdout, stderr, exitCode). Never
    /// throws for a non-zero exit — callers decide what that means (e.g. a
    /// failed `citadel status --json` before login is an expected state, not
    /// an app error).
    @discardableResult
    public func run(_ args: [String], timeout: TimeInterval = 15) throws -> (stdout: Data, stderr: Data, exitCode: Int32) {
        guard isAvailable() else { throw CLIBridgeError.binaryNotFound }

        let process = Process()
        process.executableURL = URL(fileURLWithPath: binaryPath)
        process.arguments = args

        let stdoutPipe = Pipe()
        let stderrPipe = Pipe()
        process.standardOutput = stdoutPipe
        process.standardError = stderrPipe

        try process.run()

        let deadline = Date().addingTimeInterval(timeout)
        while process.isRunning && Date() < deadline {
            Thread.sleep(forTimeInterval: 0.05)
        }
        if process.isRunning {
            process.terminate()
        }
        process.waitUntilExit()

        let stdout = stdoutPipe.fileHandleForReading.readDataToEndOfFile()
        let stderr = stderrPipe.fileHandleForReading.readDataToEndOfFile()
        return (stdout, stderr, process.terminationStatus)
    }

    /// `citadel status --json` — used for both process-only and machine-wide
    /// modes; in machine-wide mode the CLI itself attaches to the running
    /// `citadel up` backend (see the file-level comment above).
    public func fetchStatus() throws -> CitadelStatus {
        let (stdout, stderr, code) = try run(["status", "--json"])
        guard code == 0 else {
            throw CLIBridgeError.processFailed(exitCode: code, stderr: String(data: stderr, encoding: .utf8) ?? "")
        }
        do {
            return try JSONDecoder().decode(CitadelStatus.self, from: stdout)
        } catch {
            throw CLIBridgeError.decodeFailed(error)
        }
    }

    /// Process-only connect. `citadel login` is normally an interactive
    /// device-authorization flow (see internal/nexus/deviceauth.go); this app
    /// does not yet surface that flow's device code in the UI (see the PR
    /// description's "Deferred" list), so this call only succeeds cleanly
    /// against a machine already authenticated once from the CLI (i.e. state
    /// exists and `login` reconnects), and otherwise returns the CLI's own
    /// stderr for the app to display as-is.
    public func processOnlyConnect() throws {
        let (_, stderr, code) = try run(["login"], timeout: 30)
        guard code == 0 else {
            throw CLIBridgeError.processFailed(exitCode: code, stderr: String(data: stderr, encoding: .utf8) ?? "")
        }
    }

    public func processOnlyDisconnect() throws {
        let (_, stderr, code) = try run(["logout"], timeout: 15)
        guard code == 0 else {
            throw CLIBridgeError.processFailed(exitCode: code, stderr: String(data: stderr, encoding: .utf8) ?? "")
        }
    }

    public func version() -> String? {
        guard let (stdout, _, code) = try? run(["version"], timeout: 5), code == 0 else { return nil }
        return String(data: stdout, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines)
    }
}

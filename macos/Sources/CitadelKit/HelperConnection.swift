// HelperConnection.swift
// App-side wrapper for talking to CitadelHelper over XPC, and for
// registering/unregistering it via SMAppService.
//
// SMAppService.daemon(plistName:) registration requirements (enforced by the
// OS, not this code): the named plist must live in the app bundle at
// Contents/Library/LaunchDaemons/<plistName>, its Label must match the
// filename, and the app + the helper executable it points at must be signed
// by the same team. That bundle assembly is #672's packaging responsibility;
// see Resources/CitadelHelper/ai.aceteam.citadel.helper.plist in this
// package for the plist this expects to find once packaged.
import Foundation
import ServiceManagement

public enum HelperConnectionError: Error, LocalizedError {
    case notRegistered
    case xpcFailure(String)

    public var errorDescription: String? {
        switch self {
        case .notRegistered:
            return "The Citadel helper is not installed. Enable machine-wide mode in Preferences to install it."
        case .xpcFailure(let detail):
            return "Could not reach the Citadel helper: \(detail)"
        }
    }
}

@available(macOS 13.0, *)
public final class HelperConnection: @unchecked Sendable {
    public static let shared = HelperConnection()

    private let plistName = "\(HelperConstants.machServiceName).plist"
    private var service: SMAppService { SMAppService.daemon(plistName: plistName) }

    public init() {}

    public var status: SMAppService.Status { service.status }

    public var isRegistered: Bool {
        switch service.status {
        case .enabled, .requiresApproval: return true
        default: return false
        }
    }

    /// Registers the LaunchDaemon. May return with `.requiresApproval` — the
    /// caller (Preferences UI) should surface that state distinctly ("go
    /// approve this in System Settings") rather than treating it as success
    /// or failure, since it is neither.
    public func register() throws {
        try service.register()
    }

    /// Unregisters the LaunchDaemon. Callers doing a full uninstall MUST call
    /// `CitadelHelperProtocol.prepareForUninstall` first (via `withHelper`
    /// below) so routes/DNS are restored while the helper still has root —
    /// after unregistration there is no privileged process left to do it.
    public func unregister() throws {
        try service.unregister()
    }

    /// Opens a fresh XPC connection, runs `body`, then invalidates it.
    /// A fresh connection per call (rather than a long-lived one) keeps this
    /// simple and matches the low call frequency (connect/disconnect/status,
    /// not a hot path); NSXPCConnection reconnects transparently if the
    /// daemon restarts, so there is no real cost to this beyond one XPC
    /// handshake per call.
    private func withHelper<T>(_ body: (CitadelHelperProtocol, @escaping (T) -> Void) -> Void) async throws -> T {
        guard isRegistered else { throw HelperConnectionError.notRegistered }

        let connection = NSXPCConnection(machServiceName: HelperConstants.machServiceName, options: .privileged)
        connection.remoteObjectInterface = NSXPCInterface(with: CitadelHelperProtocol.self)
        connection.resume()
        defer { connection.invalidate() }

        return try await withCheckedThrowingContinuation { continuation in
            let proxy = connection.remoteObjectProxyWithErrorHandler { error in
                continuation.resume(throwing: HelperConnectionError.xpcFailure(error.localizedDescription))
            }
            guard let helper = proxy as? CitadelHelperProtocol else {
                continuation.resume(throwing: HelperConnectionError.xpcFailure("unexpected proxy type"))
                return
            }
            body(helper) { result in
                continuation.resume(returning: result)
            }
        }
    }

    public func bringUpMachineWide(nodeName: String, authKey: String?) async throws -> HelperResult {
        let request = MachineWideUpRequest(nodeName: nodeName, authKey: authKey)
        let requestData = (try? JSONEncoder().encode(request)) ?? Data()
        let resultData: Data = try await withHelper { helper, reply in
            helper.bringUpMachineWide(requestJSON: requestData, reply: reply)
        }
        return HelperResult.decode(resultData)
    }

    public func takeDownMachineWide() async throws -> HelperResult {
        let resultData: Data = try await withHelper { helper, reply in
            helper.takeDownMachineWide(reply: reply)
        }
        return HelperResult.decode(resultData)
    }

    public func checkMachineWideReadiness() async throws -> HelperResult {
        let resultData: Data = try await withHelper { helper, reply in
            helper.checkMachineWideReadiness(reply: reply)
        }
        return HelperResult.decode(resultData)
    }

    public func prepareForUninstall() async throws -> HelperResult {
        let resultData: Data = try await withHelper { helper, reply in
            helper.prepareForUninstall(reply: reply)
        }
        return HelperResult.decode(resultData)
    }

    public func ping() async throws -> String {
        try await withHelper { helper, reply in
            helper.ping(reply: reply)
        }
    }
}

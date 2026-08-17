// HelperProtocol.swift
// The XPC contract between Citadel.app (unprivileged) and CitadelHelper
// (root, via SMAppService's LaunchDaemon route — issue #670's decision A).
//
// Every method carries plain Data/String/Bool arguments rather than custom
// model types. XPC requires NSSecureCoding-conformant types end to end;
// Data/String/NSNumber already conform, so encoding requests/replies as JSON
// Data sidesteps writing and auditing a second NSSecureCoding class per
// model. The helper is a small, security-sensitive surface — fewer moving
// parts in the marshaling layer is worth the JSON-encode/decode cost.
import Foundation

/// Request payload for `bringUpMachineWide`. Mirrors the flags `cmd/up.go`'s
/// `runUp()` accepts (`--node-name`, `--authkey`).
public struct MachineWideUpRequest: Codable, Sendable {
    public let nodeName: String
    public let authKey: String?

    public init(nodeName: String, authKey: String?) {
        self.nodeName = nodeName
        self.authKey = authKey
    }
}

/// Result of a helper operation. `detail` carries the CLI's own stderr on
/// failure so the app can show the real reason (e.g. "already running",
/// "needs administrator privileges") instead of a generic error.
public struct HelperResult: Codable, Sendable {
    public let ok: Bool
    public let detail: String?

    public init(ok: Bool, detail: String?) {
        self.ok = ok
        self.detail = detail
    }

    public func encoded() -> Data { (try? JSONEncoder().encode(self)) ?? Data() }
    public static func decode(_ data: Data?) -> HelperResult {
        guard let data, let result = try? JSONDecoder().decode(HelperResult.self, from: data) else {
            return HelperResult(ok: false, detail: "helper returned no result")
        }
        return result
    }
}

/// The privileged operations the helper performs on the app's behalf. Every
/// implementation of this on the helper side MUST run only after the calling
/// connection has passed code-signature validation (see
/// CitadelHelper/HelperListenerDelegate.swift) — an XPC listener that skips
/// that check is a local privilege-escalation hole from any process able to
/// discover the mach service name (which is not a secret).
@objc public protocol CitadelHelperProtocol {
    /// Brings the machine onto the network the way `sudo citadel up` does,
    /// without the app ever needing sudo itself — the helper already runs as
    /// root once SMAppService has registered and launchd has started it.
    /// `requestJSON` is a JSON-encoded `MachineWideUpRequest`.
    func bringUpMachineWide(requestJSON: Data, reply: @escaping (Data) -> Void)

    /// Equivalent to `citadel down`.
    func takeDownMachineWide(reply: @escaping (Data) -> Void)

    /// Equivalent to `citadel up --check` — reports readiness without
    /// changing system state. Useful before offering the machine-wide
    /// toggle in Preferences.
    func checkMachineWideReadiness(reply: @escaping (Data) -> Void)

    /// Part of the Preferences > Uninstall flow (#670's "inverting the
    /// Tailscale lesson" requirement): tears down machine-wide networking if
    /// running, restores DNS/routes (`citadel down`'s CleanUpSystemState
    /// path), and reports what it did so the app can show a confirmation
    /// rather than leaving the user to guess. Does NOT unregister the
    /// SMAppService registration itself — that must happen from the
    /// unprivileged app (SMAppService.daemon(...).unregister() runs in the
    /// app's own security context), so Uninstall.swift calls this FIRST,
    /// then unregisters.
    func prepareForUninstall(reply: @escaping (Data) -> Void)

    /// Liveness probe + version string, so the app can tell "helper not
    /// installed" from "helper installed but not responding" from "helper
    /// fine".
    func ping(reply: @escaping (String) -> Void)
}

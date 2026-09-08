// Uninstall.swift
// The "one obvious Uninstall" flow #670 asks for, as the deliberate inverse
// of the Tailscale.app back-out path documented in the issue: no dedicated
// uninstall menu item, deactivation only reachable programmatically, and a
// CLI escape hatch (`systemextensionsctl uninstall`) that needs SIP disabled
// on a stock Mac.
//
// Every step here is independently visible in the returned report — the
// point is that "did it actually restore my network" is answerable by
// reading this struct, not by re-deriving it from scattered log lines.
import Foundation

public struct UninstallStep: Codable, Sendable {
    public let name: String
    public let ok: Bool
    public let detail: String?
}

public struct UninstallReport: Codable, Sendable {
    public let steps: [UninstallStep]
    public var allSucceeded: Bool { steps.allSatisfy { $0.ok } }
}

@available(macOS 13.0, *)
public final class Uninstaller: @unchecked Sendable {
    private let cli: CLIBridge
    private let helper: HelperConnection
    private let prefs: Preferences

    public init(
        cli: CLIBridge = CLIBridge(),
        helper: HelperConnection = .shared,
        prefs: Preferences = .shared
    ) {
        self.cli = cli
        self.helper = helper
        self.prefs = prefs
    }

    /// Runs the full back-out, in an order that matters: routes/DNS are
    /// restored WHILE the helper still has root and is still registered,
    /// then the helper's registration is removed, then the login item, then
    /// process-only state. Reversing this order (unregister first) would
    /// strand the machine-wide interface with nothing privileged left to
    /// tear it down — exactly the archaeology #670 calls out.
    public func uninstall() async -> UninstallReport {
        var steps: [UninstallStep] = []

        if helper.isRegistered {
            do {
                let result = try await helper.prepareForUninstall()
                steps.append(UninstallStep(
                    name: "Restore machine-wide routing and DNS",
                    ok: result.ok,
                    detail: result.detail
                ))
            } catch {
                steps.append(UninstallStep(
                    name: "Restore machine-wide routing and DNS",
                    ok: false,
                    detail: error.localizedDescription
                ))
            }

            do {
                try helper.unregister()
                steps.append(UninstallStep(name: "Remove privileged helper", ok: true, detail: nil))
            } catch {
                steps.append(UninstallStep(
                    name: "Remove privileged helper",
                    ok: false,
                    detail: error.localizedDescription
                ))
            }
        } else {
            steps.append(UninstallStep(
                name: "Restore machine-wide routing and DNS",
                ok: true,
                detail: "machine-wide mode was not installed"
            ))
        }

        if prefs.launchAtLogin {
            do {
                try prefs.setLaunchAtLogin(false)
                steps.append(UninstallStep(name: "Remove login item", ok: true, detail: nil))
            } catch {
                steps.append(UninstallStep(
                    name: "Remove login item",
                    ok: false,
                    detail: error.localizedDescription
                ))
            }
        }

        // Process-only state (userspace tsnet key + peer state) is left
        // alone deliberately unless the caller also wants a full logout —
        // uninstalling the .app should not, by itself, revoke this
        // machine's network identity, since the CLI can remain in use
        // independently of the GUI. `citadel down` is still attempted below
        // as a safety net in case machine-wide state exists outside the
        // helper's knowledge (e.g. the helper was never registered but a
        // bare `sudo citadel up` was run from a terminal previously). This
        // app process is unprivileged, so on a machine where the helper path
        // above did NOT already restore routing, this call is expected to
        // fail with citadel's own "needs administrator privileges" message —
        // that failure is reported verbatim rather than hidden, which is
        // still strictly better than the silent-success alternative.
        do {
            let (_, stderr, code) = try cli.run(["down"], timeout: 15)
            steps.append(UninstallStep(
                name: "Clear any remaining machine-wide state",
                ok: code == 0,
                detail: code == 0 ? nil : String(data: stderr, encoding: .utf8)
            ))
        } catch {
            steps.append(UninstallStep(
                name: "Clear any remaining machine-wide state",
                ok: false,
                detail: error.localizedDescription
            ))
        }

        return UninstallReport(steps: steps)
    }
}

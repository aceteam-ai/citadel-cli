// HelperListenerDelegate.swift
// Accepts (or refuses) incoming XPC connections to the privileged helper.
//
// This is the security boundary #670 calls out explicitly: "the helper must
// validate the calling app's code signature (SMAppServiceAuthorizedClients /
// audit token) or it is a local privilege-escalation hole." Any process on
// the box can discover the mach service name (it is not a secret — it is
// literally in this repo) and attempt to connect; only the code-signature
// check below stands between that and a root-level `citadel up`/`citadel
// down`.
import CitadelKit
import Foundation
import Security

final class HelperListenerDelegate: NSObject, NSXPCListenerDelegate {
    func listener(_ listener: NSXPCListener, shouldAcceptNewConnection newConnection: NSXPCConnection) -> Bool {
        guard isConnectionAuthorized(newConnection) else {
            NSLog(
                "CitadelHelper: rejected XPC connection from unauthorized caller (pid %d)",
                newConnection.processIdentifier
            )
            return false
        }

        newConnection.exportedInterface = NSXPCInterface(with: CitadelHelperProtocol.self)
        newConnection.exportedObject = HelperService()
        newConnection.invalidationHandler = {
            NSLog("CitadelHelper: connection invalidated (pid %d)", newConnection.processIdentifier)
        }
        newConnection.resume()
        return true
    }

    /// Validates that the connecting process is Citadel.app, signed by our
    /// team, using `SecCodeCopyGuestWithAttributes` — the primitive #670
    /// names directly ("SMAppServiceAuthorizedClients / audit token").
    ///
    /// KNOWN LIMITATION, worth fixing before this ships (tracked as a
    /// follow-up on #670, not silently accepted): this identifies the caller
    /// by PID (`kSecGuestAttributePid` + `connection.processIdentifier`,
    /// both public API) rather than by audit token. The audit token is the
    /// stronger identifier — it names the exact kernel-issued process
    /// instance, immune to PID reuse — but `NSXPCConnection.auditToken` is
    /// not present in the public Foundation header on the SDK this was built
    /// against (macOS 26 SDK, Xcode 26.6; only `processIdentifier` is
    /// declared in NSXPCConnection.h). Several shipped privileged-helper
    /// samples reach it anyway via KVC (`connection.value(forKey:
    /// "auditToken")`) against the private ivar, which was deliberately not
    /// done here to keep this code on documented API. The PID TOCTOU window
    /// (the original process exits and a new one is assigned the same PID
    /// between connection accept and this check) is narrow — this check runs
    /// synchronously in `shouldAcceptNewConnection`, before any privileged
    /// method executes — but it is not zero, and the code-signature
    /// requirement below only strengthens WHICH process at that PID must be,
    /// not WHEN the PID was sampled.
    private func isConnectionAuthorized(_ connection: NSXPCConnection) -> Bool {
        if CodeSigningPolicy.isPlaceholder {
            // Loudly refuse rather than silently accepting everyone: an
            // unsigned/dev build with the placeholder Team ID must fail
            // closed, not open. #672 replacing CodeSigningPolicy.expectedTeamID
            // is what turns this back on.
            NSLog("CitadelHelper: CodeSigningPolicy.expectedTeamID is still a placeholder; refusing all connections")
            return false
        }

        let pid = connection.processIdentifier
        let attributes: [String: Any] = [kSecGuestAttributePid as String: NSNumber(value: pid)]
        var code: SecCode?
        let copyStatus = SecCodeCopyGuestWithAttributes(nil, attributes as CFDictionary, [], &code)
        guard copyStatus == errSecSuccess, let code else {
            NSLog("CitadelHelper: SecCodeCopyGuestWithAttributes failed (%d)", copyStatus)
            return false
        }

        // Anchored to Apple's root, signed by our Team ID, and identified as
        // exactly our app's bundle ID — all three are required so neither a
        // re-signed impostor nor an unrelated app that happens to share our
        // Team ID (e.g. another one of our own products) can drive this.
        let requirementString = """
        identifier "\(HelperConstants.appBundleIdentifier)" \
        and anchor apple generic \
        and certificate leaf[subject.OU] = "\(CodeSigningPolicy.expectedTeamID)"
        """

        var requirement: SecRequirement?
        let reqStatus = SecRequirementCreateWithString(requirementString as CFString, [], &requirement)
        guard reqStatus == errSecSuccess, let requirement else {
            NSLog("CitadelHelper: SecRequirementCreateWithString failed (%d)", reqStatus)
            return false
        }

        let validity = SecCodeCheckValidity(code, [], requirement)
        if validity != errSecSuccess {
            NSLog("CitadelHelper: caller failed code-signature validation (%d)", validity)
            return false
        }
        return true
    }
}

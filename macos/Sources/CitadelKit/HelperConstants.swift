// HelperConstants.swift
// Identifiers shared between the app, the helper, and their Info.plist /
// LaunchDaemon plist (Resources/CitadelHelper/*.plist). Centralized so the
// XPC mach service name can never drift between the two processes that must
// agree on it.
import Foundation

public enum HelperConstants {
    /// Must equal:
    ///   - Resources/CitadelHelper/ai.aceteam.citadel.helper.plist's `Label`
    ///   - that plist's `MachServices` key
    ///   - Resources/CitadelHelper/Info.plist's `CFBundleIdentifier`
    /// #672's packaging step owns wiring these into the actual .app bundle;
    /// this constant is the single source of truth the Swift code reads.
    public static let machServiceName = "ai.aceteam.citadel.helper"

    /// The main app's bundle identifier, used by the helper to validate the
    /// calling app's code signature (see CitadelHelper's listener delegate).
    /// Must equal Resources/CitadelApp/Info.plist's `CFBundleIdentifier`.
    public static let appBundleIdentifier = "ai.aceteam.citadel"
}

// Preferences.swift
// UserDefaults-backed preferences: launch at login, connection mode, node
// name. Kept separate from AppState (which lives in the CitadelApp target)
// so it has no SwiftUI/Combine dependency and is easy to unit test.
import Foundation
import ServiceManagement

public final class Preferences: @unchecked Sendable {
    public static let shared = Preferences()

    private enum Key {
        static let connectionMode = "ai.aceteam.citadel.connectionMode"
        static let nodeName = "ai.aceteam.citadel.nodeName"
        static let launchAtLogin = "ai.aceteam.citadel.launchAtLogin"
    }

    private let defaults: UserDefaults

    public init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
    }

    public var connectionMode: ConnectionMode {
        get {
            guard let raw = defaults.string(forKey: Key.connectionMode),
                  let mode = ConnectionMode(rawValue: raw) else {
                return .processOnly
            }
            return mode
        }
        set { defaults.set(newValue.rawValue, forKey: Key.connectionMode) }
    }

    /// Empty means "use the machine hostname", matching `cmd/up.go`'s
    /// `runUp()` fallback (getSavedHostname() then os.Hostname()).
    public var nodeNameOverride: String {
        get { defaults.string(forKey: Key.nodeName) ?? "" }
        set { defaults.set(newValue, forKey: Key.nodeName) }
    }

    /// Reflects SMAppService.mainApp's actual registration status, not just
    /// a stored preference — the two can drift if the user changes it from
    /// System Settings > General > Login Items, which SMAppService does not
    /// notify us about. Callers should re-read this after returning from the
    /// background rather than trusting a cached value.
    public var launchAtLogin: Bool {
        get {
            if #available(macOS 13.0, *) {
                return SMAppService.mainApp.status == .enabled
            }
            return defaults.bool(forKey: Key.launchAtLogin)
        }
    }

    /// Throws on failure (e.g. the user needs to approve it in System
    /// Settings first run) so the UI can surface why the toggle didn't take.
    @available(macOS 13.0, *)
    public func setLaunchAtLogin(_ enabled: Bool) throws {
        if enabled {
            try SMAppService.mainApp.register()
        } else {
            try SMAppService.mainApp.unregister()
        }
        defaults.set(enabled, forKey: Key.launchAtLogin)
    }
}

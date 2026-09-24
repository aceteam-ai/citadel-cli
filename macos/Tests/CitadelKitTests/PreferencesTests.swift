// PreferencesTests.swift
import XCTest
@testable import CitadelKit

final class PreferencesTests: XCTestCase {
    private func makeIsolatedPreferences() -> Preferences {
        // A dedicated suite per test avoids polluting (or being polluted by)
        // NSUserDefaults.standard, which other tests / the app itself may
        // also touch.
        let suiteName = "ai.aceteam.citadel.tests.\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: suiteName)!
        return Preferences(defaults: defaults)
    }

    func testDefaultConnectionModeIsProcessOnly() {
        let prefs = makeIsolatedPreferences()
        XCTAssertEqual(prefs.connectionMode, .processOnly)
    }

    func testConnectionModeRoundTrips() {
        let prefs = makeIsolatedPreferences()
        prefs.connectionMode = .machineWide
        XCTAssertEqual(prefs.connectionMode, .machineWide)
    }

    func testNodeNameOverrideDefaultsToEmpty() {
        let prefs = makeIsolatedPreferences()
        XCTAssertEqual(prefs.nodeNameOverride, "")
        prefs.nodeNameOverride = "my-mac"
        XCTAssertEqual(prefs.nodeNameOverride, "my-mac")
    }
}

final class CodeSigningPolicyTests: XCTestCase {
    /// This is the guardrail the file-level comment on CodeSigningPolicy
    /// promises: a shipped build must not still carry the placeholder Team
    /// ID, or HelperListenerDelegate's "refuse everyone while placeholder"
    /// branch means the helper is permanently unusable — which is safe, but
    /// silently so. #672 flipping this test red is the intended signal that
    /// the real Team ID needs to replace it.
    func testExpectedTeamIDIsStillThePlaceholder() {
        XCTAssertTrue(
            CodeSigningPolicy.isPlaceholder,
            "CodeSigningPolicy.expectedTeamID was changed — also update this test " +
            "(and confirm HelperListenerDelegate's placeholder guard was intentionally removed)."
        )
    }
}

// CodeSigningPolicy.swift
// The Team ID the privileged helper trusts. Read this file before wiring up
// signing (#672) — the helper's authorization check
// (CitadelHelper/HelperListenerDelegate.swift) is a no-op-turned-hole if this
// stays a placeholder in a signed build: an unset/empty expected Team ID
// would make the code-requirement string malformed, and depending on how
// SecRequirementCreateWithString parses it, could accept connections it
// should reject. `HelperListenerDelegateTests` (once #672 lands a real Team
// ID here) should assert the placeholder value is never what ships.
public enum CodeSigningPolicy {
    /// Apple Developer Team ID both Citadel.app and CitadelHelper are signed
    /// with. THIS IS A PLACEHOLDER — #672 owns the actual signing identity
    /// and must replace this before a signed build is distributed. Left
    /// obviously fake (rather than empty) so a build that forgets to update
    /// it fails the "is this still the placeholder" check loudly instead of
    /// silently trusting nothing or everything.
    public static let expectedTeamID = "REPLACE_WITH_ACETEAM_TEAM_ID"

    public static var isPlaceholder: Bool {
        expectedTeamID == "REPLACE_WITH_ACETEAM_TEAM_ID"
    }
}

// CitadelStatus.swift
// Codable mirrors of the JSON `citadel status --json` already emits
// (dashboard.StatusData / dashboard.PeerInfo in cmd/status.go and
// internal/tui/dashboard). Field names/keys are copied from those Go structs'
// `json:` tags — keep this in sync if that shape changes.
import Foundation

/// One peer on the AceTeam Network, as reported by `citadel status --json`.
public struct CitadelPeer: Codable, Identifiable, Hashable, Sendable {
    public var id: String { ip.isEmpty ? hostname : ip }

    public let hostname: String
    public let ip: String
    public let online: Bool
    public let latency: String?
    public let connType: String?

    enum CodingKeys: String, CodingKey {
        case hostname, ip, online, latency
        case connType = "connType"
    }
}

/// Snapshot of this node's status, decoded from `citadel status --json`.
///
/// Only the fields the menu bar / preferences UI needs are declared;
/// `CodingKeys` intentionally omits system-vitals/GPU/service fields that
/// exist in the Go struct so a schema change there doesn't need a matching
/// change here unless the app actually wants the new field.
public struct CitadelStatus: Codable, Sendable {
    public let nodeName: String
    public let nodeIP: String
    public let connected: Bool
    public let version: String
    public let peers: [CitadelPeer]

    enum CodingKeys: String, CodingKey {
        case nodeName, nodeIP, connected, version, peers
    }

    public static let disconnected = CitadelStatus(
        nodeName: "", nodeIP: "", connected: false, version: "", peers: []
    )

    public init(nodeName: String, nodeIP: String, connected: Bool, version: String, peers: [CitadelPeer]) {
        self.nodeName = nodeName
        self.nodeIP = nodeIP
        self.connected = connected
        self.version = version
        self.peers = peers
    }
}

/// The connectivity mode a user picks in Preferences.
///
/// Mirrors the two entry points in cmd/: `citadel login` (userspace tsnet,
/// no privilege) vs `citadel up` (machine-wide TUN, needs the privileged
/// helper). See internal/network/backend.go for the backend-level version of
/// this same distinction.
public enum ConnectionMode: String, Codable, CaseIterable, Sendable {
    case processOnly
    case machineWide

    public var label: String {
        switch self {
        case .processOnly: return "Process-only (this app only, no admin needed)"
        case .machineWide: return "Machine-wide (whole computer, needs admin once)"
        }
    }
}

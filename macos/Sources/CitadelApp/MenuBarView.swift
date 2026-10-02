// MenuBarView.swift
// The MenuBarExtra dropdown content: connection state, node name + network
// IP, peer list, connect/disconnect — the "status at a glance" #670 asks for.
import AppKit
import CitadelKit
import SwiftUI

struct MenuBarView: View {
    @EnvironmentObject var appState: AppState

    /// Opens the Settings/Preferences window via the responder-chain action
    /// SwiftUI's `Settings` scene registers, rather than the
    /// `\.openSettings` environment action — that key path was only added in
    /// macOS 14, and this package's floor is 13 (SMAppService's requirement,
    /// not SwiftUI's). `sendAction` with `to: nil` walks the responder chain
    /// and is a documented no-op (returns false) if nothing along it
    /// responds, so trying both the pre- and post-Ventura selector names is
    /// safe on every supported OS version rather than needing a version
    /// branch.
    private func openPreferences() {
        for name in ["showSettingsWindow:", "showPreferencesWindow:"] {
            if NSApp.sendAction(Selector(name), to: nil, from: nil) {
                return
            }
        }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            statusHeader
            Divider()
            if appState.status.connected {
                peerList
                Divider()
            }
            if let error = appState.lastError {
                Text(error)
                    .font(.caption)
                    .foregroundStyle(.red)
                    .padding(.horizontal, 12)
                    .padding(.vertical, 6)
                Divider()
            }
            connectButton
            Divider()
            Button("Preferences…") {
                openPreferences()
            }
            .keyboardShortcut(",", modifiers: .command)
            quitMenu
        }
        .padding(.vertical, 6)
        .frame(minWidth: 260)
        .task { await appState.refreshStatus() }
    }

    private var statusHeader: some View {
        VStack(alignment: .leading, spacing: 2) {
            HStack {
                Circle()
                    .fill(appState.status.connected ? Color.green : Color.gray)
                    .frame(width: 8, height: 8)
                Text(appState.status.connected ? "Connected" : "Not Connected")
                    .font(.headline)
            }
            if appState.status.connected {
                Text(appState.status.nodeName.isEmpty ? "This machine" : appState.status.nodeName)
                    .font(.subheadline)
                if !appState.status.nodeIP.isEmpty {
                    Text(appState.status.nodeIP)
                        .font(.system(.caption, design: .monospaced))
                        .foregroundStyle(.secondary)
                }
            }
            Text(appState.connectionMode.label)
                .font(.caption2)
                .foregroundStyle(.secondary)
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 6)
    }

    private var peerList: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("PEERS")
                .font(.caption2)
                .foregroundStyle(.secondary)
                .padding(.horizontal, 12)
            if appState.status.peers.isEmpty {
                Text("No other peers online")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .padding(.horizontal, 12)
            } else {
                ForEach(appState.status.peers.prefix(8)) { peer in
                    peerRow(peer)
                }
                if appState.status.peers.count > 8 {
                    Text("+ \(appState.status.peers.count - 8) more")
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                        .padding(.horizontal, 12)
                }
            }
        }
        .padding(.vertical, 4)
    }

    private func peerRow(_ peer: CitadelPeer) -> some View {
        HStack {
            Circle()
                .fill(peer.online ? Color.green : Color.gray.opacity(0.5))
                .frame(width: 6, height: 6)
            Text(peer.hostname)
                .font(.caption)
            Spacer()
            Text(peer.ip)
                .font(.system(.caption2, design: .monospaced))
                .foregroundStyle(.secondary)
        }
        .padding(.horizontal, 12)
    }

    private var connectButton: some View {
        Button {
            Task {
                if appState.status.connected {
                    await appState.disconnect()
                } else {
                    await appState.connect()
                }
            }
        } label: {
            Text(appState.status.connected ? "Disconnect" : "Connect")
        }
        .disabled(appState.isBusy)
        .padding(.horizontal, 12)
        .padding(.vertical, 2)
    }

    /// Two explicit choices rather than one "Quit" — see #670's UX note
    /// about Tailscale's "Quit (Leave VPN Active)" being a surprising
    /// default. Here neither option is the ambiguous default: both spell out
    /// what happens to the connection.
    private var quitMenu: some View {
        Group {
            Button("Quit — Leave Network Connected") {
                NSApp.terminate(nil)
            }
            Button("Quit and Disconnect") {
                Task {
                    await appState.disconnect()
                    NSApp.terminate(nil)
                }
            }
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 2)
    }
}

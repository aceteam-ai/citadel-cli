// AppState.swift
// Drives the menu bar UI: polls status, dispatches connect/disconnect to
// either CLIBridge (process-only) or HelperConnection (machine-wide)
// depending on the user's Preferences.connectionMode.
import CitadelKit
import Foundation
import SwiftUI

@MainActor
final class AppState: ObservableObject {
    @Published private(set) var status: CitadelStatus = .disconnected
    @Published private(set) var lastError: String?
    @Published private(set) var isBusy: Bool = false
    @Published var connectionMode: ConnectionMode {
        didSet { Preferences.shared.connectionMode = connectionMode }
    }
    @Published private(set) var uninstallReport: UninstallReport?

    private let cli = CLIBridge()
    private var pollTask: Task<Void, Never>?

    /// A menu bar status poll should not be indistinguishable from a hung
    /// machine — `citadel status --json` today runs a full system-vitals
    /// collection (see CLIBridge.swift's file comment), so this interval is
    /// deliberately not aggressive. Tightening it is exactly the kind of
    /// change that should wait for a lighter status source.
    private let pollInterval: TimeInterval = 10

    init() {
        connectionMode = Preferences.shared.connectionMode
        startPolling()
    }

    deinit {
        pollTask?.cancel()
    }

    func startPolling() {
        pollTask?.cancel()
        pollTask = Task { [weak self] in
            while !Task.isCancelled {
                await self?.refreshStatus()
                try? await Task.sleep(nanoseconds: UInt64((self?.pollInterval ?? 10) * 1_000_000_000))
            }
        }
    }

    func refreshStatus() async {
        do {
            let fetched = try cli.fetchStatus()
            status = fetched
            lastError = nil
        } catch CLIBridgeError.binaryNotFound {
            lastError = "citadel not found"
            status = .disconnected
        } catch {
            // A non-zero exit from `citadel status --json` before any login
            // has happened is expected, not an error worth alarming the
            // user about in the menu bar — just reflect "disconnected".
            status = .disconnected
            lastError = nil
        }
    }

    private func resolvedNodeName() -> String {
        let override = Preferences.shared.nodeNameOverride
        if !override.isEmpty { return override }
        return ProcessInfo.processInfo.hostName
    }

    func connect() async {
        guard !isBusy else { return }
        isBusy = true
        defer { isBusy = false }

        switch connectionMode {
        case .processOnly:
            do {
                try cli.processOnlyConnect()
                lastError = nil
            } catch {
                lastError = error.localizedDescription
            }
        case .machineWide:
            guard #available(macOS 13.0, *) else {
                lastError = "Machine-wide mode needs macOS 13 or later."
                return
            }
            do {
                if !HelperConnection.shared.isRegistered {
                    try HelperConnection.shared.register()
                }
                let result = try await HelperConnection.shared.bringUpMachineWide(
                    nodeName: resolvedNodeName(),
                    authKey: nil
                )
                lastError = result.ok ? nil : (result.detail ?? "failed to bring up machine-wide mode")
            } catch {
                lastError = error.localizedDescription
            }
        }
        await refreshStatus()
    }

    /// Disconnects the active mode. #670's UX note (inverting the Tailscale
    /// "Quit (Leave VPN Active)" trap) requires the menu's Quit item to be
    /// unambiguous about whether this runs — see MenuBarView's two distinct
    /// Quit actions, only one of which calls this.
    func disconnect() async {
        guard !isBusy else { return }
        isBusy = true
        defer { isBusy = false }

        switch connectionMode {
        case .processOnly:
            do {
                try cli.processOnlyDisconnect()
                lastError = nil
            } catch {
                lastError = error.localizedDescription
            }
        case .machineWide:
            guard #available(macOS 13.0, *) else { return }
            do {
                let result = try await HelperConnection.shared.takeDownMachineWide()
                lastError = result.ok ? nil : (result.detail ?? "failed to take down machine-wide mode")
            } catch {
                lastError = error.localizedDescription
            }
        }
        await refreshStatus()
    }

    func runUninstall() async {
        guard #available(macOS 13.0, *) else { return }
        isBusy = true
        defer { isBusy = false }
        let report = await Uninstaller().uninstall()
        uninstallReport = report
        await refreshStatus()
    }
}

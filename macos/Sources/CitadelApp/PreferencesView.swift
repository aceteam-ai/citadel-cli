// PreferencesView.swift
// The Preferences window: launch at login, machine-wide vs process-only
// mode, node name, log access, and the "one obvious Uninstall" #670 asks
// for as the deliberate inverse of Tailscale's archaeology-required back-out.
import AppKit
import CitadelKit
import SwiftUI

struct PreferencesView: View {
    @EnvironmentObject var appState: AppState
    @StateObject private var viewModel = PreferencesViewModel()

    var body: some View {
        TabView {
            generalTab
                .tabItem { Label("General", systemImage: "gearshape") }
            uninstallTab
                .tabItem { Label("Uninstall", systemImage: "trash") }
        }
        .frame(width: 460, height: 340)
        .onAppear { viewModel.refresh() }
    }

    private var generalTab: some View {
        Form {
            Section {
                Toggle("Launch Citadel at login", isOn: Binding(
                    get: { viewModel.launchAtLogin },
                    set: { viewModel.setLaunchAtLogin($0) }
                ))
                if let error = viewModel.launchAtLoginError {
                    Text(error).font(.caption).foregroundStyle(.red)
                }
            }

            Section("Connection mode") {
                Picker("Mode", selection: $appState.connectionMode) {
                    ForEach(ConnectionMode.allCases, id: \.self) { mode in
                        Text(mode.label).tag(mode)
                    }
                }
                .pickerStyle(.radioGroup)
                Text(
                    appState.connectionMode == .machineWide
                        ? "The whole computer joins the network. Installs a privileged helper the first time — you'll be asked to approve it once."
                        : "Only this app joins the network. No admin access needed."
                )
                .font(.caption)
                .foregroundStyle(.secondary)
            }

            Section("Node name") {
                TextField("Defaults to this computer's name", text: Binding(
                    get: { viewModel.nodeNameOverride },
                    set: { viewModel.setNodeNameOverride($0) }
                ))
            }

            Section {
                Button("Open Logs") {
                    viewModel.openLogs()
                }
            }
        }
        .padding()
    }

    private var uninstallTab: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text("Uninstall Citadel")
                .font(.headline)
            Text(
                "Removes the privileged helper, restores any network routes and DNS " +
                "settings machine-wide mode changed, and turns off launch at login. " +
                "This does not remove the Citadel.app bundle itself — drag it to the " +
                "Trash afterward."
            )
            .font(.callout)
            .foregroundStyle(.secondary)
            .fixedSize(horizontal: false, vertical: true)

            Button(role: .destructive) {
                Task { await appState.runUninstall() }
            } label: {
                if appState.isBusy {
                    ProgressView().controlSize(.small)
                } else {
                    Text("Uninstall…")
                }
            }
            .disabled(appState.isBusy)

            if let report = appState.uninstallReport {
                uninstallReportView(report)
            }

            Spacer()
        }
        .padding()
    }

    private func uninstallReportView(_ report: UninstallReport) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(report.allSucceeded ? "Uninstall complete" : "Uninstall finished with problems")
                .font(.subheadline.bold())
                .foregroundStyle(report.allSucceeded ? .green : .orange)
            ForEach(Array(report.steps.enumerated()), id: \.offset) { _, step in
                HStack(alignment: .top, spacing: 6) {
                    Image(systemName: step.ok ? "checkmark.circle.fill" : "xmark.circle.fill")
                        .foregroundStyle(step.ok ? .green : .red)
                    VStack(alignment: .leading) {
                        Text(step.name).font(.caption)
                        if let detail = step.detail {
                            Text(detail).font(.caption2).foregroundStyle(.secondary)
                        }
                    }
                }
            }
        }
        .padding(.top, 4)
    }
}

/// Thin wrapper around Preferences (CitadelKit) so the view can bind to it.
/// Kept separate from AppState because these are settings, not live
/// connection state, and the two window types (menu bar dropdown vs
/// Preferences window) do not need to share a lifecycle.
@MainActor
final class PreferencesViewModel: ObservableObject {
    @Published var launchAtLogin: Bool = false
    @Published var launchAtLoginError: String?
    @Published var nodeNameOverride: String = ""

    private let prefs = Preferences.shared

    func refresh() {
        launchAtLogin = prefs.launchAtLogin
        nodeNameOverride = prefs.nodeNameOverride
    }

    func setLaunchAtLogin(_ enabled: Bool) {
        guard #available(macOS 13.0, *) else {
            launchAtLoginError = "Requires macOS 13 or later."
            return
        }
        do {
            try prefs.setLaunchAtLogin(enabled)
            launchAtLoginError = nil
            launchAtLogin = prefs.launchAtLogin
        } catch {
            launchAtLoginError = error.localizedDescription
            // Reflect the OS's actual state, not the requested one — see
            // Preferences.launchAtLogin's doc comment on why this can drift.
            launchAtLogin = prefs.launchAtLogin
        }
    }

    func setNodeNameOverride(_ value: String) {
        nodeNameOverride = value
        prefs.nodeNameOverride = value
    }

    func openLogs() {
        // citadel's log locations follow platform.ConfigDir()); on macOS
        // that's ~/.citadel-cli for the common (non-root) case. Reveal it in
        // Finder rather than guessing a specific log file name, since the
        // exact filename is owned by internal/platform and can change.
        let dir = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(".citadel-cli")
        NSWorkspace.shared.selectFile(nil, inFileViewerRootedAtPath: dir.path)
    }
}

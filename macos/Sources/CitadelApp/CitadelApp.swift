// CitadelApp.swift
// App entry point: a menu-bar-only SwiftUI app (no Dock icon/main window —
// see Resources/CitadelApp/Info.plist's LSUIElement), the way Tailscale.app
// presents itself. #670's Goal section.
import CitadelKit
import SwiftUI

@main
struct CitadelMenuBarApp: App {
    @StateObject private var appState = AppState()

    var body: some Scene {
        MenuBarExtra {
            MenuBarView()
                .environmentObject(appState)
        } label: {
            Image(systemName: appState.status.connected ? "shield.checkerboard" : "shield.slash")
        }
        .menuBarExtraStyle(.window)

        Settings {
            PreferencesView()
                .environmentObject(appState)
        }
    }
}

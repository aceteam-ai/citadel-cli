// swift-tools-version:5.10
// macos/Package.swift
//
// Citadel.app — the macOS menu-bar front end for citadel-cli (issue #670).
//
// This is a Swift Package rather than a checked-in .xcodeproj so `swift build`
// is a meaningful, fast CI/local check with no Xcode project file to keep in
// sync. Producing a signed, notarized, double-clickable .app bundle (proper
// Info.plist merge, entitlements, embedded LaunchDaemon plist, codesign,
// notarization) is packaging work that issue #672 owns — see Resources/ in
// this directory for the plist/entitlement inputs that step will consume.
//
// Layout:
//   Sources/CitadelKit    - shared, testable code: CLI bridge, XPC protocol,
//                            preferences, uninstall flow. No UI.
//   Sources/CitadelApp    - the SwiftUI menu-bar app (unprivileged).
//   Sources/CitadelHelper - the privileged helper skeleton (runs as root via
//                            SMAppService's LaunchDaemon route).
import PackageDescription

let package = Package(
    name: "CitadelApp",
    platforms: [
        // SMAppService (the elevation route decided in #670) requires 13.0.
        .macOS(.v13)
    ],
    products: [
        .library(name: "CitadelKit", targets: ["CitadelKit"]),
        .executable(name: "CitadelApp", targets: ["CitadelApp"]),
        .executable(name: "CitadelHelper", targets: ["CitadelHelper"]),
    ],
    targets: [
        .target(
            name: "CitadelKit",
            path: "Sources/CitadelKit"
        ),
        .executableTarget(
            name: "CitadelApp",
            dependencies: ["CitadelKit"],
            path: "Sources/CitadelApp"
        ),
        .executableTarget(
            name: "CitadelHelper",
            dependencies: ["CitadelKit"],
            path: "Sources/CitadelHelper"
        ),
        .testTarget(
            name: "CitadelKitTests",
            dependencies: ["CitadelKit"],
            path: "Tests/CitadelKitTests"
        ),
    ]
)

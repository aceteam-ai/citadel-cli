// main.swift
// CitadelHelper entry point. Installed and started by launchd via
// SMAppService.daemon(plistName:) — see
// Resources/CitadelHelper/ai.aceteam.citadel.helper.plist for the LaunchDaemon
// definition and HelperConstants.machServiceName for the mach service name
// both sides agree on. This process runs as root; the app never does.
import CitadelKit
import Foundation

NSLog("CitadelHelper: starting (mach service: %@)", HelperConstants.machServiceName)

if CodeSigningPolicy.isPlaceholder {
    NSLog(
        "CitadelHelper: WARNING — CodeSigningPolicy.expectedTeamID is a placeholder. " +
        "All XPC connections will be refused until #672 sets the real Team ID."
    )
}

let delegate = HelperListenerDelegate()
let listener = NSXPCListener(machServiceName: HelperConstants.machServiceName)
listener.delegate = delegate
listener.resume()

RunLoop.main.run()

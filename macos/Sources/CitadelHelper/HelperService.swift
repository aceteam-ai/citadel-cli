// HelperService.swift
// Implements CitadelHelperProtocol. This process runs as root (it is the
// LaunchDaemon SMAppService installed), so unlike the app it can freely
// invoke `citadel up` / `citadel down` without sudo — it effectively IS
// today's `sudo citadel up`, just started by launchd instead of a terminal.
// See cmd/up.go for the command this wraps.
import CitadelKit
import Foundation

final class HelperService: NSObject, CitadelHelperProtocol {
    private let cli = CLIBridge()
    private let stateLock = NSLock()

    /// The long-running `citadel up` child process, when we started one.
    /// `citadel up` blocks in the foreground until interrupted (see
    /// cmd/up.go's `<-ctx.Done()`), so bringing the machine up means
    /// launching it and NOT waiting for it to exit — takedown means
    /// signaling it, the GUI equivalent of the user's Ctrl-C.
    private var upProcess: Process?

    func bringUpMachineWide(requestJSON: Data, reply: @escaping (Data) -> Void) {
        DispatchQueue.global(qos: .userInitiated).async {
            reply(self.doBringUp(requestJSON: requestJSON).encoded())
        }
    }

    private func doBringUp(requestJSON: Data) -> HelperResult {
        stateLock.lock()
        if let existing = upProcess, existing.isRunning {
            stateLock.unlock()
            return HelperResult(ok: false, detail: "machine-wide mode is already running")
        }
        stateLock.unlock()

        guard let request = try? JSONDecoder().decode(MachineWideUpRequest.self, from: requestJSON) else {
            return HelperResult(ok: false, detail: "malformed request")
        }
        guard cli.isAvailable() else {
            return HelperResult(ok: false, detail: "citadel binary not found on this machine")
        }

        var args = ["up", "--node-name", request.nodeName]
        if let authKey = request.authKey, !authKey.isEmpty {
            args.append(contentsOf: ["--authkey", authKey])
        }

        let process = Process()
        process.executableURL = URL(fileURLWithPath: cli.resolvedBinaryPath)
        process.arguments = args
        let stderrPipe = Pipe()
        process.standardError = stderrPipe

        do {
            try process.run()
        } catch {
            return HelperResult(ok: false, detail: "failed to launch citadel up: \(error.localizedDescription)")
        }

        stateLock.lock()
        upProcess = process
        stateLock.unlock()

        // Poll a SEPARATE short-lived `citadel status --json` (not the child
        // we just started) for `connected == true`, since that is exactly
        // what confirms the backend published its local API socket and the
        // control plane marked the node running (see
        // internal/network/machinewide.go's waitForConnection, which this
        // mirrors from the outside).
        let deadline = Date().addingTimeInterval(30)
        while Date() < deadline {
            if !process.isRunning {
                let stderrData = stderrPipe.fileHandleForReading.readDataToEndOfFile()
                let detail = String(data: stderrData, encoding: .utf8) ?? "citadel up exited unexpectedly"
                stateLock.lock()
                upProcess = nil
                stateLock.unlock()
                return HelperResult(ok: false, detail: detail)
            }
            if let status = try? cli.fetchStatus(), status.connected {
                return HelperResult(ok: true, detail: nil)
            }
            Thread.sleep(forTimeInterval: 0.5)
        }
        return HelperResult(ok: false, detail: "timed out waiting for machine-wide mode to come up")
    }

    func takeDownMachineWide(reply: @escaping (Data) -> Void) {
        DispatchQueue.global(qos: .userInitiated).async {
            reply(self.doTakeDown().encoded())
        }
    }

    private func doTakeDown() -> HelperResult {
        stateLock.lock()
        let process = upProcess
        stateLock.unlock()

        if let process, process.isRunning {
            // SIGINT is what cmd/up.go listens for (signal.NotifyContext with
            // os.Interrupt), so this triggers the same graceful
            // network.Disconnect() path a terminal Ctrl-C would.
            process.interrupt()
            let deadline = Date().addingTimeInterval(15)
            while process.isRunning && Date() < deadline {
                Thread.sleep(forTimeInterval: 0.25)
            }
            if process.isRunning {
                // Did not exit cleanly in time; terminate and fall through to
                // `citadel down` below to clean up whatever it left behind.
                process.terminate()
            }
            stateLock.lock()
            upProcess = nil
            stateLock.unlock()
        }

        // Always run `citadel down` too, even after a clean interrupt above:
        // it is idempotent (see cmd/up.go's downCmd, which just calls
        // CleanUpSystemState unconditionally) and is the ONLY path that
        // recovers a machine-wide state left by a `citadel up` this helper
        // did not itself start (e.g. after a helper/daemon restart lost
        // track of the child).
        guard cli.isAvailable() else {
            return HelperResult(ok: false, detail: "citadel binary not found on this machine")
        }
        do {
            let (_, stderr, code) = try cli.run(["down"], timeout: 15)
            if code == 0 {
                return HelperResult(ok: true, detail: nil)
            }
            return HelperResult(ok: false, detail: String(data: stderr, encoding: .utf8))
        } catch {
            return HelperResult(ok: false, detail: error.localizedDescription)
        }
    }

    func checkMachineWideReadiness(reply: @escaping (Data) -> Void) {
        DispatchQueue.global(qos: .userInitiated).async {
            guard self.cli.isAvailable() else {
                reply(HelperResult(ok: false, detail: "citadel binary not found on this machine").encoded())
                return
            }
            do {
                let (stdout, stderr, code) = try self.cli.run(["up", "--check"], timeout: 15)
                if code == 0 {
                    reply(HelperResult(ok: true, detail: String(data: stdout, encoding: .utf8)).encoded())
                } else {
                    reply(HelperResult(ok: false, detail: String(data: stderr, encoding: .utf8)).encoded())
                }
            } catch {
                reply(HelperResult(ok: false, detail: error.localizedDescription).encoded())
            }
        }
    }

    func prepareForUninstall(reply: @escaping (Data) -> Void) {
        // Same teardown as takeDownMachineWide — kept as a distinct XPC
        // method (rather than having the app just call takeDownMachineWide)
        // so the two call sites can diverge later (e.g. uninstall wanting a
        // stricter post-teardown verification) without an app-side call site
        // silently changing behavior for the ordinary disconnect flow too.
        takeDownMachineWide(reply: reply)
    }

    func ping(reply: @escaping (String) -> Void) {
        reply(cli.version() ?? "citadel-helper (citadel binary not found)")
    }
}

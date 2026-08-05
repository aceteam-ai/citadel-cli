// internal/services/native_stop.go
package services

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// Stopping a native engine used to be `pkill -f <binary>`, which matches the
// pattern against the ENTIRE COMMAND LINE of every process and kills each match
// (#696). Stopping ollama could therefore kill an operator's `journalctl -u
// ollama`, an editor with an ollama path open, or any script whose argv merely
// mentions the engine -- the write-side of the same looseness #677 fixed on the
// read side.
//
// The replacement targets the process citadel actually started: StartNativeService
// records its PID, and stop verifies that the recorded PID is STILL the expected
// binary before signalling it. Verification is the load-bearing half. A stale
// pidfile whose PID has been recycled by an unrelated process is exactly the bug
// being fixed, so an unverified PID is treated as stale -- removed, never killed.
//
// An engine citadel did not start (systemd, a hand-run binary) must still be
// stoppable, so there is a fallback -- but a narrow one: it matches the
// EXECUTABLE NAME, not a substring of the command line, and anchors vllm's two
// python forms (`python -m vllm.entrypoints...` and the pip console script) at
// argv[0]/argv[1].
//
// Platform note: identifying and enumerating processes here uses /proc and `ps`,
// which are unix-only, exactly like the `pkill`/`pgrep` this replaced and like
// IsNativeServiceRunning still does. Windows support for the native engine path
// is a pre-existing gap and is deliberately not addressed here.

// runDirEnv overrides the directory holding <service>.pid files. It exists
// because the run dir must be identical for the process that starts an engine
// and the (possibly different) process that stops it; tests and container images
// that cannot write the default location set it explicitly.
const runDirEnv = "CITADEL_RUN_DIR"

// Stop timing. Injectable so tests do not have to wait out the real grace
// period; the shipped values are pinned by TestNativeStopTimingDefaults.
var (
	// nativeStopGrace is how long a SIGTERM'd engine gets to exit before SIGKILL.
	// Engines flush state and release VRAM on the way out, so this is generous
	// enough for an orderly exit but short enough that a stop job does not hang.
	nativeStopGrace = 5 * time.Second
	// nativeStopPoll is how often the exit is re-checked during the grace window.
	nativeStopPoll = 100 * time.Millisecond
)

// nativeRunDir returns the directory holding native-engine pidfiles.
//
// It is deliberately NOT derived from the logDir passed to StartNativeService:
// the two start call sites pass different directories (cmd/service.go uses
// ~/citadel-node/logs, the job handler uses <ConfigDir>/logs), and the stop call
// sites pass no directory at all. A pidfile only works if start and stop agree
// on where it lives, so it is anchored to the single per-node location both can
// resolve without an argument: the config dir.
func nativeRunDir() string {
	if v := strings.TrimSpace(os.Getenv(runDirEnv)); v != "" {
		return v
	}
	return filepath.Join(platform.ConfigDir(), "run")
}

// nativePidFilePath returns the pidfile path for a service.
func nativePidFilePath(serviceName string) string {
	return filepath.Join(nativeRunDir(), serviceName+".pid")
}

// writeNativePidFile records the PID of an engine citadel just started.
func writeNativePidFile(serviceName string, pid int) error {
	dir := nativeRunDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create run directory: %w", err)
	}
	return os.WriteFile(nativePidFilePath(serviceName), []byte(strconv.Itoa(pid)+"\n"), 0644)
}

// readNativePidFile returns the recorded PID, or false when there is no usable
// one. PIDs of 1 or less are rejected outright: a corrupt or truncated pidfile
// must never resolve to init.
func readNativePidFile(serviceName string) (int, bool) {
	raw, err := os.ReadFile(nativePidFilePath(serviceName))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
		return 0, false
	}
	return pid, true
}

// processEntry is one row of the process table.
type processEntry struct {
	pid     int
	cmdline string
}

// processCmdline returns the full command line of pid, and false when there is
// no such process (or nothing left to identify it by, e.g. a reaped zombie).
// Indirected through a var so tests can drive the decision logic directly.
var processCmdline = defaultProcessCmdline

func defaultProcessCmdline(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	if runtime.GOOS == "linux" {
		raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
		if err != nil {
			return "", false
		}
		args := strings.FieldsFunc(string(raw), func(r rune) bool { return r == 0 })
		if len(args) == 0 {
			// Kernel thread or a zombie: no argv to identify it with, so it is
			// not something we can confirm as our engine.
			return "", false
		}
		return strings.Join(args, " "), true
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
	if err != nil {
		return "", false
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", false
	}
	return line, true
}

// listProcesses enumerates the process table. Indirected through a var for the
// same reason as processCmdline.
var listProcesses = defaultListProcesses

func defaultListProcesses() ([]processEntry, error) {
	// -A rather than -e: both mean "every process", but -e is BSD-style
	// "show environment" on macOS while -A is portable across procps and BSD ps.
	out, err := exec.Command("ps", "-A", "-o", "pid=,args=").Output()
	if err != nil {
		return nil, err
	}
	var entries []processEntry
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pidStr, rest, found := strings.Cut(line, " ")
		if !found {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		rest = strings.TrimSpace(rest)
		if rest == "" {
			continue
		}
		entries = append(entries, processEntry{pid: pid, cmdline: rest})
	}
	return entries, nil
}

// processMatchesService reports whether a command line belongs to the service's
// own engine process.
//
// The rule is executable-name equality on argv[0], never a substring of the
// command line: `journalctl -u ollama`, `vim notes/ollama.md` and
// `citadel service stop ollama` all mention the binary and none of them is the
// engine. Multi-token AltBinaries (vllm's `python -m
// vllm.entrypoints.openai.api_server`) are matched anchored: the interpreter has
// to BE argv[0] and the remaining tokens have to be the leading arguments in
// order.
func processMatchesService(service NativeService, cmdline string) bool {
	args := strings.Fields(cmdline)
	if len(args) == 0 {
		return false
	}
	exe := filepath.Base(args[0])

	if service.Binary != "" && exe == service.Binary {
		return true
	}

	for _, alt := range service.AltBinaries {
		tokens := strings.Fields(alt)
		switch {
		case len(tokens) == 0:
			continue
		case len(tokens) == 1:
			// A plain alternative executable name; exact match only.
			// NOTE: llamacpp lists the very generic "server" here, so an
			// unrelated program literally named `server` would be stopped. That
			// is still strictly narrower than the old whole-command-line match,
			// and pruning AltBinaries is a separate change (they double as the
			// discovery list for GetNativeBinaryPath).
			if exe == tokens[0] {
				return true
			}
		default:
			if !interpreterMatches(exe, tokens[0]) {
				continue
			}
			// Console-script form. `vllm` is only ever installed by pip, so
			// /usr/local/bin/vllm is a shebang script and the kernel rewrites
			// argv to `<interpreter> /usr/local/bin/vllm serve <model>` --
			// verified locally: a `#!/bin/sh` script named vllm-ish appears in
			// the process table as `/bin/sh ./vllm-ish serve mymodel`. Neither
			// the primary-name check (argv[0] is the interpreter) nor the -m
			// comparison below sees that, so without this branch the one engine
			// that motivated the anchored pattern would become unstoppable.
			// Still anchored: the interpreter must BE argv[0] and the binary must
			// BE the script at argv[1], so `vim /usr/local/bin/vllm` does not
			// match.
			if service.Binary != "" && len(args) >= 2 && filepath.Base(args[1]) == service.Binary {
				return true
			}
			if len(args)-1 < len(tokens)-1 {
				continue
			}
			matched := true
			for i, tok := range tokens[1:] {
				if args[1+i] != tok {
					matched = false
					break
				}
			}
			if matched {
				return true
			}
		}
	}
	return false
}

// interpreterMatches accepts the versioned names a scripting interpreter really
// appears under -- python3, python3.11 -- for an AltBinaries token of "python",
// while still rejecting anything else (pythonish-wrapper, mypython).
func interpreterMatches(exe, token string) bool {
	if exe == token {
		return true
	}
	if !strings.HasPrefix(exe, token) {
		return false
	}
	for _, r := range exe[len(token):] {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}

// isServiceProcess reports whether pid is currently a live process belonging to
// this service. This is the verification gate: everything that signals a PID
// goes through it first.
func isServiceProcess(pid int, service NativeService) bool {
	cmdline, ok := processCmdline(pid)
	if !ok {
		return false
	}
	return processMatchesService(service, cmdline)
}

// stopVerifiedPID signals a PID that has already been verified as the service,
// escalating to SIGKILL if it does not exit within the grace period.
//
// Aliveness during the wait is re-tested with the same identity check rather
// than a bare existence probe, which handles both an unreaped zombie (no argv
// left, therefore gone for our purposes) and the nastier case of the PID being
// recycled inside the grace window -- escalating to SIGKILL against a recycled
// PID would be the very bug this change removes.
func stopVerifiedPID(pid int, service NativeService) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("failed to stop service: %w", err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return fmt.Errorf("failed to stop service: %w", err)
	}

	deadline := time.Now().Add(nativeStopGrace)
	for {
		if !isServiceProcess(pid, service) {
			return nil
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(nativeStopPoll)
	}

	if !isServiceProcess(pid, service) {
		return nil
	}
	if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("failed to kill service after %s: %w", nativeStopGrace, err)
	}
	return nil
}

// stopMatchingProcesses is the fallback for an engine citadel did not start.
// Finding nothing is success: "already stopped" is the desired end state, which
// also preserves the old behaviour of treating pkill's "no process found" as OK.
func stopMatchingProcesses(service NativeService) error {
	entries, err := listProcesses()
	if err != nil {
		return fmt.Errorf("failed to stop service: could not list processes: %w", err)
	}
	self := os.Getpid()
	var firstErr error
	for _, entry := range entries {
		if entry.pid <= 1 || entry.pid == self {
			continue
		}
		if !processMatchesService(service, entry.cmdline) {
			continue
		}
		if err := stopVerifiedPID(entry.pid, service); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// StopNativeService stops a running native service.
//
// Order matters: the recorded PID first (the process citadel owns), and the
// name-matching sweep only when there is no usable pidfile. Stopping the engine
// citadel started is the contract; sweeping every matching process on top of
// that would widen the blast radius again for no gain.
//
// This deliberately never consults the service's port. A wedged engine that has
// stopped answering is still a process holding VRAM and must still be killed --
// see the comments on IsNativeServiceRunning and on serviceStop (#649/#677).
func StopNativeService(serviceName string) error {
	service, ok := NativeServices[serviceName]
	if !ok {
		return fmt.Errorf("unknown service: %s", serviceName)
	}

	if pid, ok := readNativePidFile(serviceName); ok {
		if isServiceProcess(pid, service) {
			if err := stopVerifiedPID(pid, service); err != nil {
				return err
			}
			_ = os.Remove(nativePidFilePath(serviceName))
			return nil
		}
		// The PID is dead, or -- the dangerous case -- has been recycled and now
		// belongs to something unrelated. Either way the pidfile is stale: drop
		// it and fall through to the name match. Never signal an unverified PID.
		_ = os.Remove(nativePidFilePath(serviceName))
	}

	return stopMatchingProcesses(service)
}

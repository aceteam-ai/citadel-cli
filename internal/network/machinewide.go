// internal/network/machinewide.go
// Entry point for machine-wide (TUN) mode — `citadel up` (issue #643).
package network

import (
	"context"
	"fmt"
	"runtime"
)

// ErrNeedsElevation is returned when machine-wide mode is requested without
// the privileges to create a network interface.
//
// This is deliberately an error and NOT a fallback to userspace. A user who
// ran `citadel up` asked for their whole machine to be on the mesh; quietly
// giving them a process-scoped connection instead would leave them believing
// `ssh 100.64.0.5` should work when it never will.
type ErrNeedsElevation struct{ Hint string }

func (e *ErrNeedsElevation) Error() string {
	return "machine-wide mode needs administrator privileges: " + e.Hint
}

// ElevationHint is the platform-appropriate way to re-run elevated.
func ElevationHint() string {
	switch runtime.GOOS {
	case "windows":
		return "run citadel from an Administrator prompt"
	default:
		return "run 'sudo citadel up'"
	}
}

// ConnectMachineWide brings up a real TUN interface so the whole machine
// routes the mesh, and publishes a local API socket other citadel processes
// attach to (see attachedBackend).
//
// It takes the same state directory — and therefore the same node identity —
// as `citadel login`. That is the point: one machine is one node. The
// state-dir lock is what prevents a second backend from opening the same
// identity concurrently.
func ConnectMachineWide(ctx context.Context, config ServerConfig, elevated bool) (*NetworkServer, error) {
	if !elevated {
		return nil, &ErrNeedsElevation{Hint: ElevationHint()}
	}

	stateDir := config.StateDir
	if stateDir == "" {
		stateDir = GetStateDir()
	}

	if _, err := EnsureStateDir(); err != nil {
		return nil, fmt.Errorf("failed to create state directory: %w", err)
	}

	// A machine-wide backend is already up: nothing to do, and starting a
	// second one would fight the first for the interface and the node key.
	if localAPIReachable(LocalAPISocketPath(stateDir)) {
		return nil, fmt.Errorf("machine-wide mode is already running on this host (use 'citadel down' to stop it)")
	}

	if err := checkNoUserspaceHolders(stateDir); err != nil {
		return nil, err
	}

	// No-op on non-Windows. On Windows, extracts + hash-verifies + pre-loads
	// the embedded wintun driver from the executable's own directory, and
	// refuses (rather than falling back to userspace) if that directory is
	// writable by a non-administrator -- see EnsureWintunDriver.
	if err := ensureWintunDriverIfNeeded(); err != nil {
		return nil, err
	}

	s := &NetworkServer{
		controlURL: config.ControlURL,
		hostname:   config.Hostname,
		stateDir:   stateDir,
		mode:       ModeTUN,
		backend: newTUNBackend(ServerConfig{
			Hostname:   config.Hostname,
			ControlURL: config.ControlURL,
		}, stateDir, config.AuthKey),
	}

	if err := s.backend.Up(ctx); err != nil {
		s.backend = nil
		s.releaseStateLock()
		return nil, err
	}

	if err := s.waitForConnection(ctx); err != nil {
		s.backend.Close()
		s.backend = nil
		s.releaseStateLock()
		return nil, err
	}

	FixStatePermissions()

	// Record where this node's state actually lives. `citadel up` is elevated
	// and typically run interactively first (where $SUDO_USER makes resolution
	// correct), then later as a launchd/systemd service — which has no
	// $SUDO_USER and would otherwise resolve root's home, open an empty state
	// dir, and register a SECOND node for this machine. Writing the pointer
	// here converges the service run on the identity established now.
	if err := EnsureMachineStatePointer(stateDir); err != nil {
		// Non-fatal: the node is up and correct for THIS process. Only the
		// later service run would be at risk, and that is worth a warning
		// rather than refusing a working connection.
		logf("warning: could not record machine state pointer (a later service run may register a duplicate node): %v", err)
	}

	s.connected = true
	SetGlobal(s)
	return s, nil
}

// MachineWideRunning reports whether a `citadel up` currently holds the mesh
// on this host. Cheap enough to call from status paths.
func MachineWideRunning() bool {
	return localAPIReachable(LocalAPISocketPath(GetStateDir()))
}

// PreflightResult describes whether this machine can run machine-wide mode.
type PreflightResult struct {
	Elevated bool `json:"elevated"`

	// DriverOK reports whether the platform's network driver is ready to
	// load. On Windows (issue #709) this covers extracting, hash-verifying,
	// and pre-loading the embedded wintun driver -- including the
	// install-directory ACL check, so a wrong install location (e.g.
	// %LOCALAPPDATA%\Citadel instead of %ProgramFiles%\Citadel) is reported
	// distinctly from a missing-privileges failure via Detail. Trivially
	// true on Linux/macOS, which have no external driver file.
	DriverOK bool `json:"driver_ok"`

	DeviceOK  bool   `json:"device_ok"`
	Device    string `json:"device,omitempty"`
	Detail    string `json:"detail,omitempty"`
	AlreadyUp bool   `json:"already_up"`
}

// PreflightMachineWide answers "can this box do machine-wide mode?" without
// changing any *routing/DNS* state that outlives the call.
//
// It creates the network interface and immediately closes it again. It does
// NOT start the engine, install routes, or touch DNS — so it is safe to run
// on a machine already carrying other VPN software, and safe to kill (there
// is no routing/DNS state to leave behind).
//
// Windows exception: ensureWintunDriverIfNeeded (below) extracts wintun.dll
// to disk next to citadel.exe and loads it into this process before the
// interface check runs. That disk write and in-process load are real,
// harmless, idempotent side effects (re-extraction always re-verifies the
// hash) -- but they are NOT nothing, so "no state to strand" above refers
// only to the interface/routes/DNS, not the driver file.
//
// This exists because the failure that actually bites users is per-platform
// and happens at exactly this step: a missing wintun.dll on Windows, no
// /dev/net/tun on a container host, a Linux binary without CAP_NET_ADMIN.
// Reporting that precisely beats a wall of engine output.
func PreflightMachineWide(elevated bool) PreflightResult {
	res := PreflightResult{
		Elevated:  elevated,
		AlreadyUp: MachineWideRunning(),
	}
	if !elevated {
		res.Detail = ElevationHint()
		return res
	}

	// Windows-only in practice (no-op elsewhere): extracts, hash-verifies,
	// and pre-loads the embedded wintun driver, including the
	// install-directory ACL check. Checked BEFORE tstunNew so a
	// wrong-install-location failure is reported as exactly that, not as a
	// generic "could not create the device" error from deeper in wintun's
	// own loader.
	if err := ensureWintunDriverIfNeeded(); err != nil {
		res.Detail = err.Error()
		return res
	}
	res.DriverOK = true

	name := DefaultTUNName()
	dev, devName, err := tstunNew(name)
	if err != nil {
		res.Detail = err.Error()
		return res
	}
	dev.Close()

	res.DeviceOK = true
	res.Device = devName
	return res
}

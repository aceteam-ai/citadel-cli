// cmd/worker_agent.go
//
// The doctor's worker lines (citadel-cli#1867 C3 on Windows, #1903 M3 on
// macOS). On macOS the worker is the per-user LaunchAgent that
// service.LaunchAgent owns; these functions only read its state.
package cmd

import (
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/service"
)

// windowsWorkerChecks are the "Autostart" and "Worker" lines for the Windows
// supervised worker.
func windowsWorkerChecks(uw userWorkerState, exe string, ok, warn func(what, detail string)) {
	switch {
	case uw.AutostartErr != nil:
		warn("Autostart", fmt.Sprintf("could not read the sign-in entry: %v", uw.AutostartErr))
	case uw.Autostart.Set && (uw.Autostart.DisabledByUser || !uw.Autostart.PointsAtThis):
		warn("Autostart", describeUserAutostart(uw.Autostart))
	default:
		ok("Autostart", describeUserAutostart(uw.Autostart))
	}
	if uw.Running {
		ok("Worker", fmt.Sprintf("a background worker is running (supervisor PID %d) and connects to AceTeam on its own, separately from local mode. Stop it: %s worker stop", uw.SupervisorPID, exe))
	} else {
		ok("Worker", "no background worker is running (it is only needed to use this computer from AceTeam)")
	}
}

// userWorkerStateFromAgent maps a LaunchAgent state (macOS) onto the doctor's
// worker state.
func userWorkerStateFromAgent(st service.LaunchAgentState) userWorkerState {
	return userWorkerState{
		Autostart:     service.AutostartStatusFromAgent(st),
		AutostartErr:  st.Err,
		Running:       st.PID > 0,
		SupervisorPID: st.PID,
		Agent:         &st,
	}
}

// agentChecks are the doctor's "Autostart" and "Worker" lines for the macOS
// LaunchAgent. They read launchd, never change it, and carry no log content,
// only the log size: launchd appends to its two files forever.
func agentChecks(uw userWorkerState, exe string, ok, warn func(what, detail string)) {
	st := *uw.Agent
	switch {
	case st.Err != nil:
		warn("Autostart", fmt.Sprintf("could not read the LaunchAgent: %v", st.Err))
	case !st.PlistPresent:
		ok("Autostart", "Autostart is off (no LaunchAgent file at "+st.PlistPath+").")
	case st.Disabled:
		warn("Autostart", "The Citadel worker is switched off in System Settings > General > Login Items & Extensions, so macOS will not start it. Switch it on there.")
	case !st.PointsAtThis:
		warn("Autostart", fmt.Sprintf("The LaunchAgent starts %s, not this citadel. Run %s worker autostart enable to point it at this one.", st.Exe, exe))
	default:
		ok("Autostart", "Autostart is on: macOS starts "+st.Exe+" work at sign-in.")
	}
	switch {
	case st.PID > 0:
		ok("Worker", fmt.Sprintf("a background worker is running (LaunchAgent, PID %d) and connects to AceTeam on its own, separately from local mode. Stop it: %s worker stop", st.PID, exe))
	case st.Loaded:
		warn("Worker", "the LaunchAgent is loaded but its worker is not running right now (macOS restarts it; if it keeps failing, see ~/Library/Logs/citadel/citadel-error.log)")
	case st.PlistPresent && !st.Disabled:
		warn("Worker", "the LaunchAgent is installed but not running. It starts at your next sign-in, or run: "+exe+" service start")
	default:
		ok("Worker", "no background worker is running (it is only needed to use this computer from AceTeam)")
	}
	if st.LogBytes > service.LaunchAgentLogMaxBytes {
		warn("Worker log", fmt.Sprintf("the worker's log files in ~/Library/Logs/citadel are %d MB. They are emptied the next time the worker starts. To empty them now, run: %s worker stop; %s service start", st.LogBytes>>20, exe, exe))
	}
}

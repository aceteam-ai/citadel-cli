// cmd/local_setup_worker.go
//
// "Keep this PC available to AceTeam" on the setup page (citadel-cli#1867 C5).
// After this page connected the PC and saved its co-browse-only posture, the
// button turns on sign-in autostart (`citadel worker autostart enable`, with
// its ownership check) and starts `citadel work --background`. The setup
// process first drops its own AceTeam Network connection, which the enrollment
// left up, so the worker is the only endpoint for this node's key. The page
// never shows the launcher's output; each step is fixed copy.
//
// On macOS (citadel-cli#1903 M3) the same three steps install and start the
// per-user LaunchAgent (service.LaunchAgent, no supervisor of ours: launchd
// restarts it). On Linux it returns the service guidance instead.
package cmd

import (
	"context"
	"errors"
	"log"
	"os/exec"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/localviewer"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/service"
)

const (
	workerStepHandOff   = "Hand the AceTeam connection to the worker"
	workerStepAutostart = "Start at sign-in"
	workerStepStart     = "Start the worker now"
)

const (
	workerRefusedCopy = "Citadel starts the worker from this page only after connecting this PC here with the co-browse-only setting saved. " +
		"Run \"citadel work --background\" in a terminal instead."
	// macOS copy: the worker is the LaunchAgent, not `work --background`.
	workerMacRefusedCopy   = "Citadel starts the worker from this page only after connecting this Mac here with the co-browse-only setting saved. Run \"citadel service install\" in Terminal instead."
	workerMacHandOffFail   = "Citadel could not release this page's connection, so the worker was not started. Close this window, then try again."
	workerMacAutostartOK   = "Citadel will start when you sign in (a Login Items entry for this user)."
	workerMacAutostartFail = "Citadel could not set itself to start at sign-in. Run \"citadel worker autostart enable\" in Terminal to see why."
	workerMacStartOKCopy   = "The worker is running in the background, restarted by macOS if it stops. Stop it any time with Disconnect this PC or \"citadel worker stop\"."
	workerMacStartSkipCopy = "Not started, because the sign-in entry above was not written."
	workerMacStartFailCopy = "The worker did not start. Run \"citadel worker start\" in Terminal to see why."

	workerOtherOSCopy = "On Linux, keep the worker running with \"citadel service install\" in a terminal."

	workerHandOffOKCopy     = "This page no longer holds the connection."
	workerHandOffFailCopy   = "Citadel could not release this page's connection, so the worker was not started. Close this window, then run \"citadel work --background\" in a terminal."
	workerAutostartOKCopy   = "Citadel will start when you sign in."
	workerAutostartFailCopy = "Citadel could not set itself to start at sign-in. Run \"citadel worker autostart enable\" in a terminal to see why."
	workerStartOKCopy       = "The worker is running in the background. Stop it any time with Disconnect this PC or \"citadel worker stop\"."
	workerStartFailCopy     = "The worker did not start. Run \"citadel work --background\" in a terminal to see why."

	workerDoneCopy    = "This PC is available to AceTeam agents: co-browse on public websites only, unless you allow more."
	workerPartialCopy = "Not every step finished. Check the steps below."
)

// workerStartTimeout bounds the launcher, which itself waits up to a few
// seconds for the supervisor to take its lock.
const workerStartTimeout = 30 * time.Second

// setupWorkerDeps are the worker start's side effects, as seams.
type setupWorkerDeps struct {
	goos string
	// offered reports that this page connected the PC and saved its posture.
	offered func() bool
	// registered reports that this PC still has its AceTeam registration (a
	// Disconnect since the connect removes it).
	registered func() bool
	// postureHolds re-reads the permissions file: the public-only guard must
	// still be on.
	postureHolds    func() bool
	releaseNetwork  func() error
	enableAutostart func() error
	start           func(ctx context.Context) error
}

// startSetupWorker keeps this PC available. It refuses before any step unless
// the page connected the PC and the posture still holds.
func startSetupWorker(d setupWorkerDeps) (localviewer.SetupActionResult, error) {
	if d.goos != "windows" && d.goos != "darwin" {
		return localviewer.SetupActionResult{OK: true, Message: workerOtherOSCopy}, nil
	}
	mac := d.goos == "darwin"
	pick := func(win, darwin string) string {
		if mac {
			return darwin
		}
		return win
	}
	if !d.offered() || !d.registered() || !d.postureHolds() {
		return localviewer.SetupActionResult{}, errors.New(pick(workerRefusedCopy, workerMacRefusedCopy))
	}
	res := localviewer.SetupActionResult{OK: true}
	add := func(name, status, msg string) {
		if status == stepFailed {
			res.OK = false
		}
		res.Steps = append(res.Steps, localviewer.SetupStep{Name: name, Status: status, Message: msg})
	}

	if err := d.releaseNetwork(); err != nil {
		log.Printf("[setup] worker: release the network connection: %v", err)
		add(workerStepHandOff, stepFailed, pick(workerHandOffFailCopy, workerMacHandOffFail))
		res.Message = workerPartialCopy
		return res, nil
	}
	add(workerStepHandOff, stepOK, workerHandOffOKCopy)

	autostartOK := true
	if err := d.enableAutostart(); err != nil {
		autostartOK = false
		log.Printf("[setup] worker: autostart: %v", err)
		add(workerStepAutostart, stepFailed, pick(workerAutostartFailCopy, workerMacAutostartFail))
	} else {
		add(workerStepAutostart, stepOK, pick(workerAutostartOKCopy, workerMacAutostartOK))
	}

	if mac && !autostartOK {
		// Nothing to load without the plist; the failure above is the answer.
		add(workerStepStart, stepSkipped, workerMacStartSkipCopy)
		res.OK = false
		res.Message = workerPartialCopy
		return res, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), workerStartTimeout)
	defer cancel()
	if err := d.start(ctx); err != nil {
		log.Printf("[setup] worker: start: %v", err)
		add(workerStepStart, stepFailed, pick(workerStartFailCopy, workerMacStartFailCopy))
	} else {
		add(workerStepStart, stepOK, pick(workerStartOKCopy, workerMacStartOKCopy))
	}

	if res.OK {
		res.Message = workerDoneCopy
	} else {
		res.Message = workerPartialCopy
	}
	return res, nil
}

// productionSetupWorkerDeps uses this executable and the node config dir
// resolved at click time.
func productionSetupWorkerDeps(goos, exe string, offered func() bool) setupWorkerDeps {
	if goos == "darwin" {
		return darwinSetupWorkerDeps(exe, offered)
	}
	return setupWorkerDeps{
		goos:            goos,
		offered:         offered,
		registered:      hasNodeRegistration,
		postureHolds:    func() bool { return setupPostureHolds(nodeConfigDirFn()) },
		releaseNetwork:  network.Disconnect,
		enableAutostart: func() error { return enableUserAutostart(exe, autostartExeOwnership, service.EnableUserAutostart) },
		start: func(ctx context.Context) error {
			out, err := exec.CommandContext(ctx, exe, service.BackgroundWorkArgs...).CombinedOutput()
			if err != nil {
				log.Printf("[setup] worker launcher output: %s", out)
			}
			return err
		},
	}
}

// darwinSetupWorkerDeps is the macOS worker start: autostart writes the
// per-user LaunchAgent plist (refused as root or for a binary this user does
// not own) and start loads it. The setup process's own network connection is
// released first, as on Windows.
func darwinSetupWorkerDeps(exe string, offered func() bool) setupWorkerDeps {
	return setupWorkerDeps{
		goos:            "darwin",
		offered:         offered,
		registered:      hasNodeRegistration,
		postureHolds:    func() bool { return setupPostureHolds(nodeConfigDirFn()) },
		releaseNetwork:  network.Disconnect,
		enableAutostart: func() error { return service.EnableUserAutostart(exe) },
		start:           func(ctx context.Context) error { return service.StartUserWorker() },
	}
}

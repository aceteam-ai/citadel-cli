// cmd/local_setup_worker.go
//
// "Keep this PC available to AceTeam" on the setup page (citadel-cli#1867 C5).
// After this page connected the PC and saved its co-browse-only posture, the
// button turns on sign-in autostart (`citadel worker autostart enable`, with
// its ownership check) and starts `citadel work --background`. The setup
// process first drops its own AceTeam Network connection, which the enrollment
// left up, so the worker is the only endpoint for this node's key. The page
// never shows the launcher's output; each step is fixed copy. On Linux and
// macOS it returns the service guidance instead.
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
	workerOtherOSCopy = "On Linux and macOS, keep the worker running with \"citadel service install\" in a terminal."

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
	if d.goos != "windows" {
		return localviewer.SetupActionResult{OK: true, Message: workerOtherOSCopy}, nil
	}
	if !d.offered() || !d.registered() || !d.postureHolds() {
		return localviewer.SetupActionResult{}, errors.New(workerRefusedCopy)
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
		add(workerStepHandOff, stepFailed, workerHandOffFailCopy)
		res.Message = workerPartialCopy
		return res, nil
	}
	add(workerStepHandOff, stepOK, workerHandOffOKCopy)

	if err := d.enableAutostart(); err != nil {
		log.Printf("[setup] worker: autostart: %v", err)
		add(workerStepAutostart, stepFailed, workerAutostartFailCopy)
	} else {
		add(workerStepAutostart, stepOK, workerAutostartOKCopy)
	}

	ctx, cancel := context.WithTimeout(context.Background(), workerStartTimeout)
	defer cancel()
	if err := d.start(ctx); err != nil {
		log.Printf("[setup] worker: start: %v", err)
		add(workerStepStart, stepFailed, workerStartFailCopy)
	} else {
		add(workerStepStart, stepOK, workerStartOKCopy)
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

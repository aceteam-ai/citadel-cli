package cmd

import (
	"fmt"
	"os"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/tui"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// enrollmentTier is the local-only classification used by the zero-argument
// entrypoint. It intentionally does not dial a control plane: deciding what a
// bare command should do must work while a node is offline, and must not mint
// credentials as a side effect (citadel-cli#1116).
type enrollmentTier string

const (
	enrollmentUnenrolled enrollmentTier = "unenrolled"
	enrollmentMesh       enrollmentTier = "mesh-enrolled"
	enrollmentPlatform   enrollmentTier = "platform-enrolled"
)

// bareCommandAction is kept separate from execution so the enrollment-state
// contract remains testable without a terminal, tsnet state, or device config.
type bareCommandAction string

const (
	bareCommandStatus       bareCommandAction = "status"
	bareCommandGuidedEnroll bareCommandAction = "guided-enroll"
	bareCommandSession      bareCommandAction = "session"
)

// enrollmentTierFor is the pure three-tier classifier from #1109's design of
// record. A mesh state without platform credentials is still an enrolled node:
// S2 will give it a presence-only session rather than sending it through
// enrollment again.
func enrollmentTierFor(hasMeshState, hasDeviceCredentials bool) enrollmentTier {
	if !hasMeshState {
		return enrollmentUnenrolled
	}
	if !hasDeviceCredentials {
		return enrollmentMesh
	}
	return enrollmentPlatform
}

// bareCommandActionFor decides the S1 behavior. Non-interactive invocations
// are deliberately observational: CI, systemd, pipes, and shell scripts must
// never trigger a QR flow or start a foreground session merely by running
// `citadel`.
func bareCommandActionFor(isTTY bool, tier enrollmentTier) bareCommandAction {
	if !isTTY {
		return bareCommandStatus
	}
	if tier == enrollmentUnenrolled {
		return bareCommandGuidedEnroll
	}
	return bareCommandSession
}

// bareCommandStatusLine is a one-screen, stable non-TTY result. Session
// start/attach is intentionally deferred to S2/S3, so S1 reports only the
// enrollment fact it can establish locally.
func bareCommandStatusLine(tier enrollmentTier) string {
	return fmt.Sprintf("Citadel enrollment: %s", tier)
}

var (
	bareStdoutIsTTYFn            = tui.IsTTY
	bareStdinIsTTYFn             = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	bareHasMeshStateFn           = network.HasState
	bareHasDeviceCredentialsFn   = hasDeviceConfigured
	bareResolveControlURLFn      = network.ResolveControlURL
	bareRunEnrollCommandFn       = func() { enrollCmd.Run(enrollCmd, nil) }
	bareRunGuidedEnrollFn        = runExistingGuidedEnroll
	bareRunSessionDispatchStubFn = runEnrolledSessionDispatchStub
)

// bareInvocationIsInteractive is deliberately stricter than tui.IsTTY: the
// device-authorisation flow reads stdin, so a pipe feeding stdin is not a safe
// interactive invocation even if stdout happens to be a terminal. Treat it as
// a status-only invocation instead of stealing or blocking the pipe.
func bareInvocationIsInteractive() bool {
	return bareStdoutIsTTYFn() && bareStdinIsTTYFn()
}

// runBareCitadel is the sole implementation of bare `citadel` (S1 of #1109).
// Expert subcommands keep their own Cobra Run functions and do not pass
// through here.
func runBareCitadel(cmd *cobra.Command, _ []string) {
	tier := enrollmentTierFor(bareHasMeshStateFn(), bareHasDeviceCredentialsFn())
	switch bareCommandActionFor(bareInvocationIsInteractive(), tier) {
	case bareCommandStatus:
		fmt.Fprintln(cmd.OutOrStdout(), bareCommandStatusLine(tier))
	case bareCommandGuidedEnroll:
		// Reuse the existing QR/device-auth command rather than the control
		// center's optional Login/Continue Offline prompt. A fresh bare machine
		// must go straight into guided enrollment, not offer an offline detour.
		bareRunGuidedEnrollFn()
	case bareCommandSession:
		// S0 made the persisted control URL the one reconnect source of truth.
		// Pass that value into the S1 run/attach seam; do not reintroduce a
		// compiled production default here. S2/S3 replace this stub with the
		// durable presence/worker session and client attach transport.
		bareRunSessionDispatchStubFn(tier, bareResolveControlURLFn())
	}
}

// runExistingGuidedEnroll delegates to the established `citadel enroll` QR
// flow. That command owns the device-auth protocol, config persistence and
// mesh join; this dispatcher deliberately contributes none of them.
func runExistingGuidedEnroll() {
	bareRunEnrollCommandFn()

	// A successful device-auth flow joined the mesh and persisted device
	// credentials. Reclassify rather than assuming that outcome, then take the
	// same S1 session seam as a node that was enrolled before this invocation.
	// (The defensive no-op preserves the enrollment command's own behavior if a
	// future implementation returns without joining.)
	tier := enrollmentTierFor(bareHasMeshStateFn(), bareHasDeviceCredentialsFn())
	if tier != enrollmentUnenrolled {
		bareRunSessionDispatchStubFn(tier, bareResolveControlURLFn())
	}
}

// runEnrolledSessionDispatchStub preserves today's foreground control-center
// behavior while making the future run/attach boundary explicit. It must not
// create a worker, authenticate, or mint credentials: those are S2/S3 work.
func runEnrolledSessionDispatchStub(tier enrollmentTier, controlURL string) {
	Debug("bare dispatcher selected %s session via persisted control URL %s", tier, controlURL)
	runControlCenter()
}

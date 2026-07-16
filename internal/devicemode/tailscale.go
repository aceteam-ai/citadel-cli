// System-tailscale babysitting for device mode (#5959).
//
// A device runs the REAL Tailscale client (system daemon or the macOS App
// Store app), not citadel's embedded tsnet — laptops already have it and the
// user's other traffic policies live there. Device mode only needs three
// things from it: where the binary is, whether the session is healthy, and a
// way to re-run login with a fresh authkey.
package devicemode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// MacAppStoreTailscale is the CLI binary bundled inside the macOS App Store /
// standalone GUI variant of Tailscale, which does not install `tailscale`
// onto PATH.
const MacAppStoreTailscale = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"

// tailscaleBinEnv overrides binary discovery (tests, exotic installs).
const tailscaleBinEnv = "CITADEL_TAILSCALE_BIN"

// FindTailscale locates the tailscale CLI: env override, then PATH, then the
// macOS app-bundle variant.
func FindTailscale() (string, error) {
	return findTailscale(runtime.GOOS, exec.LookPath, os.Stat, os.Getenv(tailscaleBinEnv))
}

// findTailscale is the testable core of FindTailscale.
func findTailscale(
	goos string,
	lookPath func(string) (string, error),
	stat func(string) (os.FileInfo, error),
	envOverride string,
) (string, error) {
	if envOverride != "" {
		if _, err := stat(envOverride); err != nil {
			return "", fmt.Errorf("%s=%s: %w", tailscaleBinEnv, envOverride, err)
		}
		return envOverride, nil
	}
	if path, err := lookPath("tailscale"); err == nil {
		return path, nil
	}
	if goos == "darwin" {
		if _, err := stat(MacAppStoreTailscale); err == nil {
			return MacAppStoreTailscale, nil
		}
	}
	return "", fmt.Errorf(
		"tailscale CLI not found — install Tailscale (https://tailscale.com/download) " +
			"or set " + tailscaleBinEnv)
}

// TailscaleStatus is the subset of `tailscale status --json` device mode
// reasons about.
type TailscaleStatus struct {
	// BackendState is tailscale's engine state: "Running", "NeedsLogin",
	// "Stopped", "Starting", "NoState".
	BackendState string `json:"BackendState"`
	Self         struct {
		// KeyExpiry is the node key's expiry; nil/zero when the key does not
		// expire. RFC3339 in the JSON output.
		KeyExpiry *time.Time `json:"KeyExpiry"`
	} `json:"Self"`
}

// Status runs `tailscale status --json` and parses the fields device mode
// needs.
func Status(ctx context.Context, bin string) (*TailscaleStatus, error) {
	out, err := exec.CommandContext(ctx, bin, "status", "--json").Output()
	if err != nil {
		// `tailscale status` exits non-zero in some unhealthy states while
		// still printing valid JSON (e.g. logged out); prefer parsing it.
		if len(out) == 0 {
			return nil, fmt.Errorf("tailscale status: %w", err)
		}
	}
	return ParseStatusJSON(out)
}

// ParseStatusJSON parses `tailscale status --json` output.
func ParseStatusJSON(out []byte) (*TailscaleStatus, error) {
	var st TailscaleStatus
	if err := json.Unmarshal(out, &st); err != nil {
		return nil, fmt.Errorf("parse tailscale status JSON: %w", err)
	}
	if st.BackendState == "" {
		return nil, fmt.Errorf("tailscale status JSON missing BackendState")
	}
	return &st, nil
}

// Up (re-)authenticates the system tailscale against the mesh coordinator
// with a fresh org authkey. forceReauth rotates the node key in place, which
// is required both to recover a broken session and to extend an expiring one.
//
// The authkey travels on argv, which every tailscale CLI version accepts
// (file:/env: indirections are version-dependent and fail as a LITERAL key on
// older CLIs — a silent server-side rejection, worse than brief ps
// visibility of a single-use key that expires within the hour).
func Up(ctx context.Context, bin, loginServer string, authkey string, forceReauth bool) error {
	args := []string{"up", "--login-server=" + loginServer, "--authkey=" + authkey}
	if forceReauth {
		args = append(args, "--force-reauth")
	}
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tailscale up failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

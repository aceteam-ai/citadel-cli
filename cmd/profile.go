package cmd

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/aceteam-ai/citadel-cli/internal/hpc"
	"github.com/aceteam-ai/citadel-cli/internal/hpcscope"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

// envProfile selects a run profile when --profile is not given.
const envProfile = "CITADEL_PROFILE"

var (
	profileFlag      string
	hpcStateRootFlag string
)

// activeHPCLayout is non-nil once the HPC profile has been installed for this
// process. It is written once, in the root pre-run, before any command body.
var activeHPCLayout atomic.Pointer[hpc.Layout]

// hpcProfileActive reports whether this process runs under the HPC profile.
func hpcProfileActive() bool { return activeHPCLayout.Load() != nil }

// currentHPCLayout returns the installed layout. Only valid when active.
func currentHPCLayout() hpc.Layout {
	if l := activeHPCLayout.Load(); l != nil {
		return *l
	}
	return hpc.Layout{}
}

// resolveProfileSelection decides the run profile from the flag and the
// environment. An explicit flag wins, but a flag and an environment value that
// disagree are refused rather than silently picking one. Unknown names are
// refused: a typo must never fall back to an ordinary node.
func resolveProfileSelection(flagValue string, flagSet bool, envValue string) (string, error) {
	flagValue = strings.ToLower(strings.TrimSpace(flagValue))
	envValue = strings.ToLower(strings.TrimSpace(envValue))
	value := envValue
	if flagSet {
		if envValue != "" && envValue != flagValue {
			return "", fmt.Errorf("--profile %q and %s=%q disagree", flagValue, envProfile, envValue)
		}
		value = flagValue
	}
	switch value {
	case "", "default":
		return "", nil
	case hpc.Profile:
		return hpc.Profile, nil
	default:
		return "", fmt.Errorf("unknown profile %q (supported: %s)", value, hpc.Profile)
	}
}

// hpcProfileAllowsCommand is the positive list of commands that may run under
// the HPC profile. Everything else (login, init, control center, local mode,
// mcp, service management, ...) is refused before it can touch host state.
func hpcProfileAllowsCommand(path string) bool {
	switch path {
	case "citadel work", "citadel version":
		return true
	}
	return isHPCSubcommand(path)
}

// isHPCSubcommand reports whether path is `citadel hpc` or below it.
func isHPCSubcommand(path string) bool {
	return path == "citadel hpc" || strings.HasPrefix(path, "citadel hpc ")
}

// resolveHPCActivation is the side-effect-light core of profile activation: it
// validates the platform, command and caller, resolves the job layout and
// creates it with owner-only permissions. It does not install any process
// scope, so tests can call it repeatedly with t.TempDir() roots.
func resolveHPCActivation(cmdPath, goos string, euid int, stateRootFlag string, getenv func(string) string) (hpc.Layout, error) {
	if goos == "windows" {
		return hpc.Layout{}, hpc.ErrUnsupportedPlatform
	}
	if !hpcProfileAllowsCommand(cmdPath) {
		return hpc.Layout{}, fmt.Errorf("%q is not available in the %s profile", cmdPath, hpc.Profile)
	}
	if euid == 0 {
		return hpc.Layout{}, errors.New("the hpc profile refuses to run as root")
	}
	stateRoot := strings.TrimSpace(stateRootFlag)
	if stateRoot == "" {
		stateRoot = getenv(hpc.EnvStateRoot)
	}
	layout, err := hpc.ResolveLayout(hpc.Inputs{
		JobID:        getenv(hpc.EnvJobID),
		LegacyJobID:  getenv(hpc.EnvLegacyJobID),
		RestartCount: getenv(hpc.EnvRestartCount),
		StateRoot:    stateRoot,
		TmpDir:       getenv(hpc.EnvTmpDir),
	})
	if err != nil {
		return hpc.Layout{}, err
	}
	if err := hpc.Prepare(layout, euid); err != nil {
		return hpc.Layout{}, err
	}
	return layout, nil
}

// installHPCProfile makes the layout authoritative for the rest of the
// process (see hpcscope.Install) and turns automatic updates off.
func installHPCProfile(layout hpc.Layout) error {
	if err := hpcscope.Install(layout); err != nil {
		return err
	}
	noAutoUpdate = true
	if err := os.Setenv("CITADEL_NO_AUTO_UPDATE", "1"); err != nil {
		return err
	}
	activeHPCLayout.Store(&layout)
	return nil
}

// applyRunProfile runs first in the root pre-run. It returns handled=true when
// a profile took over startup, in which case the ordinary pre-run (update
// recovery, update check, local-mode and other exceptions) must not run.
func applyRunProfile(cmd *cobra.Command, args []string) (bool, error) {
	flagSet := false
	if f := cmd.Root().PersistentFlags().Lookup("profile"); f != nil {
		flagSet = f.Changed
	}
	profile, err := resolveProfileSelection(profileFlag, flagSet, os.Getenv(envProfile))
	if err != nil {
		return false, err
	}
	// The hpc subtree always runs under the hpc profile.
	if isHPCSubcommand(cmd.CommandPath()) && profile == "" {
		profile = hpc.Profile
	}
	if profile == "" {
		return false, nil
	}
	layout, err := resolveHPCActivation(cmd.CommandPath(), runtime.GOOS, os.Geteuid(), hpcStateRootFlag, os.Getenv)
	if err != nil {
		return false, err
	}
	if err := installHPCProfile(layout); err != nil {
		return false, fmt.Errorf("install hpc profile: %w", err)
	}
	// Private CA trust reads only an explicit --ca-cert / CITADEL_CA_CERT and
	// job-local state now that the scope is installed. It must still run
	// before the first TLS root load for a self-hosted control plane.
	if err := preparePrivateCATrust(cmd); err != nil {
		return false, fmt.Errorf("configure private CA trust: %w", err)
	}
	network.SetLogf(Debug)
	if noColorGlobal || os.Getenv("NO_COLOR") != "" {
		color.NoColor = true
	}
	Log("command: %s (profile %s, job root %s)", invocationSummary(cmd, args), hpc.Profile, layout.Root)
	return true, nil
}

func init() {
	rootCmd.PersistentFlags().StringVar(&profileFlag, "profile", "", "Run profile: hpc for a job-scoped batch allocation (or set "+envProfile+")")
	rootCmd.PersistentFlags().StringVar(&hpcStateRootFlag, "hpc-state-root", "", "Directory the hpc profile creates its job root in (default $"+hpc.EnvStateRoot+", then $TMPDIR)")
}

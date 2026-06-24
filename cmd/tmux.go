// cmd/tmux.go
package cmd

import (
	"fmt"
	"runtime"

	"github.com/aceteam-ai/citadel-cli/internal/tmux"
	"github.com/aceteam-ai/citadel-cli/internal/tmuxinstall"
	"github.com/spf13/cobra"
)

var tmuxInstallForce bool

var tmuxCmd = &cobra.Command{
	Use:   "tmux",
	Short: "Manage the Citadel-managed tmux binary for persistent terminal sessions",
	Long: `Persistent, reconnect-resilient terminal sessions ("chat to a node") are
backed by tmux. Citadel never assumes tmux is installed: it looks for tmux via
CITADEL_TMUX_BIN, then PATH, then a Citadel-managed binary. These commands manage
that Citadel-managed binary.

If no usable tmux is found, the terminal server falls back to a bare,
non-persistent shell.`,
}

var tmuxStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show how tmux is resolved on this node",
	RunE:  runTmuxStatus,
}

var tmuxInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Download, verify, and install a Citadel-managed tmux binary",
	Long: `Download a Citadel-managed static tmux build for this platform, verify its
SHA-256 checksum, and install it to the Citadel-managed path so persistent
terminal sessions work without a system tmux.

The downloaded artifact's checksum is always verified before install, and the
binary is never executed as part of verification.

Not every platform has a vetted static tmux artifact yet (macOS and Windows in
particular). On gated platforms this command reports that no artifact is
available; install tmux via your package manager instead (e.g. on macOS:
'brew install tmux').`,
	RunE: runTmuxInstall,
}

func init() {
	rootCmd.AddCommand(tmuxCmd)
	tmuxCmd.AddCommand(tmuxStatusCmd)
	tmuxCmd.AddCommand(tmuxInstallCmd)
	tmuxInstallCmd.Flags().BoolVar(&tmuxInstallForce, "force", false, "Reinstall even if a managed tmux binary already exists")
}

func runTmuxStatus(cmd *cobra.Command, args []string) error {
	managed := tmux.ManagedBinaryPath()
	fmt.Printf("Platform:            %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Printf("Managed binary path: %s\n", managed)

	if path, err := tmux.Resolve(); err == nil {
		fmt.Printf("Resolved tmux:       %s\n", path)
	} else {
		fmt.Printf("Resolved tmux:       (none) — terminals fall back to a bare shell\n")
	}

	if tmuxinstall.Available() {
		fmt.Printf("Managed artifact:    available for this platform (run 'citadel tmux install')\n")
	} else {
		if src, ok := tmuxinstall.CurrentSource(); ok {
			fmt.Printf("Managed artifact:    not available (gated) — %s\n", src.Note)
		} else {
			fmt.Printf("Managed artifact:    not supported on this platform\n")
		}
	}
	return nil
}

func runTmuxInstall(cmd *cobra.Command, args []string) error {
	inst := tmuxinstall.New()

	if inst.AlreadyInstalled() && !tmuxInstallForce {
		fmt.Printf("Managed tmux already installed at %s (use --force to reinstall).\n", inst.DestPath())
		return nil
	}

	if !tmuxinstall.Available() {
		if src, ok := tmuxinstall.CurrentSource(); ok {
			return fmt.Errorf("no managed tmux artifact for %s/%s: %s", runtime.GOOS, runtime.GOARCH, src.Note)
		}
		return fmt.Errorf("managed tmux is not supported on %s/%s; install tmux via your package manager so it is found on PATH", runtime.GOOS, runtime.GOARCH)
	}

	fmt.Printf("Installing Citadel-managed tmux to %s ...\n", inst.DestPath())
	if err := inst.Install(); err != nil {
		return fmt.Errorf("install tmux: %w", err)
	}
	fmt.Printf("Installed and checksum-verified tmux at %s\n", inst.DestPath())
	return nil
}

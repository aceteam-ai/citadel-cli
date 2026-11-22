// cmd/worker_cmd.go
//
// `citadel worker`: control the per-user background worker on Windows
// (citadel-cli#1867 C3). `autostart` manages the HKCU Run value that starts
// `citadel work --background` at logon; `stop` ends the supervisor and its
// worker. The setup page calls the same functions.
package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/service"
	"github.com/aceteam-ai/citadel-cli/internal/update"
	"github.com/aceteam-ai/citadel-cli/internal/worklock"
	"github.com/spf13/cobra"
)

var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Control the per-user background worker (Windows)",
	Long: `Control the background worker that 'citadel work --background' starts.

On Windows a standard user cannot install the Citadel service. Instead the
worker runs in the background under a small supervisor that restarts it, and an
optional per-user autostart entry starts it again when you sign in. On Linux and
macOS use 'citadel service install'.`,
}

var workerAutostartCmd = &cobra.Command{
	Use:   "autostart",
	Short: "Start the background worker when you sign in (Windows)",
}

var workerAutostartEnableCmd = &cobra.Command{
	Use:          "enable",
	Short:        "Start 'citadel work --background' at sign-in for this user",
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		exe, err := citadelExecutable()
		if err != nil {
			return fmt.Errorf("resolve the citadel executable: %w", err)
		}
		if err := enableUserAutostart(exe, autostartExeOwnership, service.EnableUserAutostart); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Autostart enabled: %s will start at sign-in.\nIt does not start now; run '%s work --background' to start it.\n", exe, quoteExe(exe))
		return nil
	},
}

// autostartExeOwnership is the doctor's self-update owner check (a test seam).
var autostartExeOwnership = update.ExecutableOwnership

// enableUserAutostart refuses to persist an executable this user does not own:
// whoever owns it could replace what runs at every sign-in. The Run value
// builder separately refuses relative, UNC and device paths.
func enableUserAutostart(exe string, ownership func(string) (update.OwnerInfo, error), write func(string) error) error {
	if _, err := service.BuildRunValue(exe); err != nil {
		return err
	}
	info, err := ownership(exe)
	if err != nil {
		return fmt.Errorf("autostart: could not verify who owns %s: %w", exe, err)
	}
	if !info.Matches() {
		return fmt.Errorf("autostart: %s is owned by %s, not by you (%s), so it will not be started at sign-in. Fix: reinstall citadel as yourself, or from an elevated PowerShell run: icacls \"%s\" /setowner \"%s\"", exe, info.OwnerName, info.UserName, exe, info.UserName)
	}
	return write(exe)
}

var workerAutostartDisableCmd = &cobra.Command{
	Use:          "disable",
	Short:        "Remove the sign-in autostart entry",
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := service.DisableUserAutostart(); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Autostart disabled. A running background worker keeps running; stop it with 'citadel worker stop'.")
		return nil
	},
}

var workerAutostartStatusCmd = &cobra.Command{
	Use:          "status",
	Short:        "Show whether the sign-in autostart entry is set",
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		exe, _ := citadelExecutable()
		st, err := service.UserAutostart(exe)
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), describeUserAutostart(st))
		return nil
	},
}

var workerStopCmd = &cobra.Command{
	Use:          "stop",
	Short:        "Stop the background worker and its supervisor",
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		nodeDir, err := userWorkerNodeDir()
		if err != nil {
			return err
		}
		return stopUserWorker(nodeDir, userWorkerStopTimeout, cmd.OutOrStdout())
	},
}

func init() {
	workerAutostartCmd.AddCommand(workerAutostartEnableCmd, workerAutostartDisableCmd, workerAutostartStatusCmd)
	workerCmd.AddCommand(workerAutostartCmd, workerStopCmd)
	rootCmd.AddCommand(workerCmd)
}

// describeUserAutostart is the one-line state for status and doctor.
func describeUserAutostart(st service.UserAutostartStatus) string {
	switch {
	case !st.Set:
		return "Autostart is off (no " + service.UserAutostartValueName + " sign-in entry)."
	case st.DisabledByUser:
		return "Autostart is set but switched off in Settings > Apps > Startup (or Task Manager), so it will not run."
	case st.PointsAtThis:
		return "Autostart is on: " + st.Value
	case st.Exe == "":
		return "Autostart entry is set but unreadable: " + st.Value + ". Run 'citadel worker autostart enable' to rewrite it."
	default:
		return "Autostart is on but starts a different citadel: " + st.Value + ". Run 'citadel worker autostart enable' to point it at this one."
	}
}

// userWorkerStopTimeout covers the supervisor's own stop grace, a kill and a
// margin.
var userWorkerStopTimeout = 2*service.DefaultSupervisorStopGrace + 10*time.Second

// userWorkerPoll is how often stop checks for the supervisor to exit.
var userWorkerPoll = 250 * time.Millisecond

// userSupervisorHeld probes a supervisor lock without taking it (a test seam).
var userSupervisorHeld = worklock.IsHeldFile

// stopUserWorker asks the supervisor holding nodeDir's lock to stop, with a
// request that names its PID, and waits until that PID no longer holds it.
func stopUserWorker(nodeDir string, timeout time.Duration, out io.Writer) error {
	lockPath := service.SupervisorLockPath(nodeDir)
	held, pid := userSupervisorHeld(lockPath)
	if !held || pid <= 0 {
		fmt.Fprintln(out, "No background worker is running for this user.")
		return nil
	}
	stopPath := service.SupervisorStopPath(nodeDir, pid)
	if err := os.WriteFile(stopPath, []byte(fmt.Sprintf("stop requested by PID %d\n", os.Getpid())), 0o600); err != nil {
		return fmt.Errorf("request stop: %w", err)
	}
	fmt.Fprintf(out, "Stopping the background worker (supervisor PID %d)...\n", pid)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		held, holder := userSupervisorHeld(lockPath)
		switch {
		case !held:
			fmt.Fprintln(out, "Stopped.")
			return nil
		case holder != pid:
			return fmt.Errorf("supervisor PID %d stopped, but PID %d now runs the background worker (it started meanwhile); run 'citadel worker stop' again", pid, holder)
		}
		time.Sleep(userWorkerPoll)
	}
	return errors.New("the background worker did not stop in time (supervisor PID " + fmt.Sprint(pid) + "); end it in Task Manager")
}

// userWorkerState is what `citadel local doctor` reports about the per-user
// worker. Reading it never changes anything.
type userWorkerState struct {
	Autostart     service.UserAutostartStatus
	AutostartErr  error
	Running       bool
	SupervisorPID int
}

// readUserWorkerState reads the Run value and probes the supervisor lock.
func readUserWorkerState(exe, nodeDir string) userWorkerState {
	st, err := service.UserAutostart(exe)
	held, pid := userSupervisorHeld(service.SupervisorLockPath(nodeDir))
	return userWorkerState{Autostart: st, AutostartErr: err, Running: held, SupervisorPID: pid}
}

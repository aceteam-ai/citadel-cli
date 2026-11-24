// cmd/local_disconnect.go
//
// "Disconnect this PC" on the setup page and `citadel local disconnect`
// (citadel-cli#1867 C5). disconnectThisPC runs, in order: stop the per-user
// background worker, remove its sign-in autostart, log this PC out of the
// AceTeam Network, and remove the device credentials the enrollment saved,
// reporting each step in fixed copy. If the worker could not be stopped or its
// autostart could not be removed, the logout and the credential removal are
// skipped: clearing state under a live (or about to restart) worker is unsafe.
// The credentials go because a later worker would otherwise keep taking jobs
// over the AceTeam API with them, mesh or not. Local mode keeps working;
// local-mode saved logins, the local audit log and the permissions file are
// never touched.
package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/localviewer"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/nodesession"
	"github.com/aceteam-ai/citadel-cli/internal/service"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// Step names, in the order they run.
const (
	disconnectStepStop      = "Stop the background worker"
	disconnectStepAutostart = "Remove sign-in autostart"
	disconnectStepLogout    = "Log out of the AceTeam Network"
	disconnectStepCreds     = "Remove this PC's AceTeam device key"
)

// Step statuses (localviewer.SetupStep.Status).
const (
	stepOK            = "ok"
	stepFailed        = "failed"
	stepSkipped       = "skipped"
	stepNotApplicable = "not_applicable"
)

// Fixed copy for each step outcome.
const (
	disconnectStopOKCopy     = "The background worker is stopped (or was not running)."
	disconnectStopNACopy     = "This system has no per-user background worker. If Citadel runs as a service here, stop it with \"citadel service stop\"."
	disconnectStopFailedCopy = "Citadel could not stop the background worker. End it in Task Manager, then press Disconnect again."
	// macOS: the worker is a LaunchAgent. A bootout that fails stops the
	// disconnect here, so a KeepAlive worker never outlives its credentials.
	disconnectStopFailedMacCopy = "Citadel could not stop its macOS background worker (a LaunchAgent), so nothing else was changed. Run \"citadel worker stop\" in Terminal to see why, then press Disconnect again."
	disconnectStopOKMacCopy     = "The background worker is stopped (or was not running), and macOS will not restart it."
	disconnectAutoOKCopy        = "Citadel no longer starts when you sign in."
	disconnectAutoNACopy        = "This system has no sign-in autostart entry for Citadel."
	disconnectAutoFailedCopy    = "Citadel could not remove its sign-in autostart entry. Run \"citadel worker autostart disable\" in a terminal."
	disconnectLogoutSkipCopy    = "Skipped, because the worker may still be running or start again at sign-in. Fix the step above, then press Disconnect again."
	disconnectLogoutNoneCopy    = "This PC was not connected to the AceTeam Network."
	disconnectLogoutOKCopy      = "Logged out. AceTeam removed this PC from your account."
	disconnectLogoutLocalCopy   = "Logged out on this PC. AceTeam was not asked to remove it from your account, so it may still be listed there; you can remove it in your AceTeam account."
	disconnectLogoutHeldCopy    = "Another Citadel program on this PC still uses its AceTeam connection (a worker, a service or \"citadel up\"). Stop it, then press Disconnect again."
	disconnectLogoutFailCopy    = "Citadel could not log out. Run \"citadel logout\" in a terminal."
	disconnectCredsOKCopy       = "The device key AceTeam gave this PC is removed, so a later worker cannot take AceTeam jobs."
	disconnectCredsNoneCopy     = "This PC had no saved AceTeam device key."
	disconnectCredsFailCopy     = "Citadel could not remove this PC's AceTeam device key. Run \"citadel logout\" in a terminal, then remove this PC from your AceTeam account."
	disconnectCredsSkipCopy     = "Skipped, because the logout did not finish."

	disconnectDoneCopy = "This PC is disconnected from AceTeam. Local mode keeps working, and your saved logins were not touched."
	// disconnectPostureNote is added when the co-browse-only marker stays.
	disconnectPostureNote = " This PC keeps its co-browse-only setting, so if you connect it again (here or with \"citadel enroll\") it stays co-browse only. " +
		"To remove that setting, run \"citadel permission setup-posture disable\" in a terminal."
	disconnectPartialCopy = "Disconnect did not finish. Check the steps below."
)

// disconnectPostureKept reports whether the co-browse-only marker is in
// place (a test seam).
var disconnectPostureKept = func() bool { return config.SetupPostureActive(nodePermissionsDir()) }

// logoutOutcome is what logoutThisNode did.
type logoutOutcome struct {
	// NotConnected: there was nothing to log out of.
	NotConnected bool
	// Deregistered: AceTeam confirmed removing this PC.
	Deregistered bool
}

// errLogoutHeld is logoutThisNode's refusal while another process holds the
// network state.
var errLogoutHeld = errors.New("another process holds this node's network state")

// disconnectDeps are Disconnect's side effects, as seams. stopWorker and
// disableAutostart return applicable=false where this OS has no such thing.
type disconnectDeps struct {
	// goos selects the copy ("" and "windows" are the Windows copy).
	goos             string
	stopWorker       func() (applicable bool, err error)
	disableAutostart func() (applicable bool, err error)
	logout           func() (logoutOutcome, error)
	// removeCredentials removes the device credentials saved by this node's
	// enrollment; removed is false when there were none.
	removeCredentials func() (removed bool, err error)
	// postureKept reports the co-browse-only marker, which Disconnect keeps
	// (nil: not reported).
	postureKept func() bool
}

// disconnectThisPC runs every step in order and reports each. A failed worker
// stop or autostart removal skips the logout, and a logout that did not
// finish skips the credential removal.
func disconnectThisPC(d disconnectDeps) localviewer.SetupActionResult {
	res := localviewer.SetupActionResult{OK: true}
	add := func(name, status, msg string) {
		if status == stepFailed || status == stepSkipped {
			res.OK = false
		}
		res.Steps = append(res.Steps, localviewer.SetupStep{Name: name, Status: status, Message: msg})
	}

	stopped := true
	switch applicable, err := d.stopWorker(); {
	case err != nil:
		log.Printf("[setup] disconnect: stop worker: %v", err)
		stopped = false
		if d.goos == "darwin" {
			add(disconnectStepStop, stepFailed, disconnectStopFailedMacCopy)
		} else {
			add(disconnectStepStop, stepFailed, disconnectStopFailedCopy)
		}
	case !applicable:
		add(disconnectStepStop, stepNotApplicable, disconnectStopNACopy)
	case d.goos == "darwin":
		add(disconnectStepStop, stepOK, disconnectStopOKMacCopy)
	default:
		add(disconnectStepStop, stepOK, disconnectStopOKCopy)
	}

	switch applicable, err := d.disableAutostart(); {
	case err != nil:
		log.Printf("[setup] disconnect: remove autostart: %v", err)
		stopped = false
		add(disconnectStepAutostart, stepFailed, disconnectAutoFailedCopy)
	case !applicable:
		add(disconnectStepAutostart, stepNotApplicable, disconnectAutoNACopy)
	default:
		add(disconnectStepAutostart, stepOK, disconnectAutoOKCopy)
	}

	loggedOut := false
	if !stopped {
		add(disconnectStepLogout, stepSkipped, disconnectLogoutSkipCopy)
	} else {
		out, err := d.logout()
		loggedOut = err == nil
		switch {
		case errors.Is(err, errLogoutHeld):
			log.Printf("[setup] disconnect: logout refused: %v", err)
			add(disconnectStepLogout, stepFailed, disconnectLogoutHeldCopy)
		case err != nil:
			log.Printf("[setup] disconnect: logout: %v", err)
			add(disconnectStepLogout, stepFailed, disconnectLogoutFailCopy)
		case out.NotConnected:
			add(disconnectStepLogout, stepOK, disconnectLogoutNoneCopy)
		case out.Deregistered:
			add(disconnectStepLogout, stepOK, disconnectLogoutOKCopy)
		default:
			add(disconnectStepLogout, stepOK, disconnectLogoutLocalCopy)
		}
	}

	if !loggedOut {
		add(disconnectStepCreds, stepSkipped, disconnectCredsSkipCopy)
	} else {
		switch removed, err := d.removeCredentials(); {
		case err != nil:
			log.Printf("[setup] disconnect: remove device credentials: %v", err)
			add(disconnectStepCreds, stepFailed, disconnectCredsFailCopy)
		case !removed:
			add(disconnectStepCreds, stepOK, disconnectCredsNoneCopy)
		default:
			add(disconnectStepCreds, stepOK, disconnectCredsOKCopy)
		}
	}

	if res.OK {
		res.Message = disconnectDoneCopy
	} else {
		res.Message = disconnectPartialCopy
	}
	if d.postureKept != nil && d.postureKept() {
		res.Message += disconnectPostureNote
	}
	return res
}

// disconnectLoggedOut reports whether res's logout step succeeded.
func disconnectLoggedOut(res localviewer.SetupActionResult) bool {
	for _, s := range res.Steps {
		if s.Name == disconnectStepLogout {
			return s.Status == stepOK
		}
	}
	return false
}

// logoutDeps are logoutThisNode's side effects, as seams.
type logoutDeps struct {
	hasState    func() bool
	isConnected func() bool
	// precheck refuses while another process holds the network state.
	precheck   func() error
	deregister func(ctx context.Context) error
	// localLogout disconnects this process and clears the network state.
	localLogout      func() error
	clearSessionMode func() error
}

// logoutThisNode is `citadel logout --force` without its prompt, printing or
// exit: a best-effort deregistration, then the local logout, then clearing the
// saved session mode. A deregistration failure (a PC enrolled from the setup
// page has no API key) is reported in the outcome, not as an error.
func logoutThisNode(ctx context.Context, d logoutDeps) (logoutOutcome, error) {
	if !d.hasState() && !d.isConnected() {
		return logoutOutcome{NotConnected: true}, nil
	}
	if err := d.precheck(); err != nil {
		return logoutOutcome{}, fmt.Errorf("%w: %v", errLogoutHeld, err)
	}
	derr := d.deregister(ctx)
	if derr != nil {
		log.Printf("[setup] logout: AceTeam did not confirm removing this PC: %v", derr)
	}
	if err := completeRequiredDeregistration(derr, false, d.localLogout); err != nil {
		return logoutOutcome{}, err
	}
	if err := d.clearSessionMode(); err != nil {
		log.Printf("[setup] logout: could not clear the prior session mode: %v", err)
	}
	return logoutOutcome{Deregistered: derr == nil}, nil
}

// removeEnrollmentCredentials removes from configFile the device credentials
// an enrollment saved (device_api_token and the fields saved beside it, or a
// legacy redis_url), through the enrollment rollback helper: the token passed
// is the one the file holds now, so the helper removes exactly those fields
// and never touches operator or node keys (aceteam_api_key, nexus_url,
// node_config_dir, ...). removed is false when there were none.
func removeEnrollmentCredentials(configFile string) (bool, error) {
	data, err := os.ReadFile(configFile)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var cur map[string]interface{}
	if err := yaml.Unmarshal(data, &cur); err != nil {
		return false, fmt.Errorf("parse %s: %w", configFile, err)
	}
	str := func(k string) string { v, _ := cur[k].(string); return v }
	token := &nexus.TokenResponse{
		DeviceAPIToken: str("device_api_token"),
		APIBaseURL:     str("api_base_url"),
		OrgID:          str("org_id"),
		OrgName:        str("org_name"),
		UserEmail:      str("user_email"),
		UserName:       str("user_name"),
		FabricNodeID:   str("fabric_node_id"),
		RedisURL:       str("redis_url"),
	}
	if token.DeviceAPIToken == "" && token.RedisURL == "" {
		return false, nil
	}
	if err := rollbackEnrollmentCredentials(configFile, token); err != nil {
		return false, err
	}
	return true, nil
}

// removeDeviceCredentialsIn removes the enrollment's device credentials from
// <dir>/config.yaml in every dir the worker reads them from
// (deviceConfigDirs: the node config dir, then the legacy invoker config dir),
// so no fallback is left for a later worker. Each dir is tried; the first
// error is returned after all were tried.
func removeDeviceCredentialsIn(dirs []string) (bool, error) {
	removed := false
	var firstErr error
	seen := map[string]bool{}
	for _, dir := range dirs {
		if dir == "" || seen[filepath.Clean(dir)] {
			continue
		}
		seen[filepath.Clean(dir)] = true
		r, err := removeEnrollmentCredentials(filepath.Join(dir, "config.yaml"))
		removed = removed || r
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return removed, firstErr
}

// hasNodeRegistration reports whether this PC holds an AceTeam Network
// registration or connection.
func hasNodeRegistration() bool {
	return network.HasState() || network.IsGlobalConnected()
}

// productionDisconnectDeps are the real side effects.
func productionDisconnectDeps() disconnectDeps {
	d := productionDisconnectDepsBase()
	if runtime.GOOS == "darwin" {
		a, err := service.NewUserLaunchAgent()
		d = darwinDisconnectDeps(d, a, err)
	}
	return d
}

func productionDisconnectDepsBase() disconnectDeps {
	return disconnectDeps{
		stopWorker: func() (bool, error) {
			if runtime.GOOS != "windows" {
				return false, nil
			}
			dir, err := userWorkerNodeDir()
			if err != nil {
				return true, err
			}
			return true, stopUserWorker(dir, userWorkerStopTimeout, io.Discard)
		},
		disableAutostart: func() (bool, error) {
			err := service.DisableUserAutostart()
			if errors.Is(err, service.ErrUserAutostartUnsupported) {
				return false, nil
			}
			return true, err
		},
		removeCredentials: func() (bool, error) { return removeDeviceCredentialsIn(deviceConfigDirs()) },
		postureKept:       disconnectPostureKept,
		logout: func() (logoutOutcome, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			return logoutThisNode(ctx, logoutDeps{
				hasState:    network.HasState,
				isConnected: network.IsGlobalConnected,
				precheck:    grantEnrollmentPrecheck,
				deregister: func(ctx context.Context) error {
					return deregisterFromBackend(ctx, os.Getenv("CITADEL_API_KEY"))
				},
				localLogout: network.Logout,
				clearSessionMode: func() error {
					err := os.Remove(nodesession.Path(network.GetNodeConfigDir()))
					if os.IsNotExist(err) {
						return nil
					}
					return err
				},
			})
		},
	}
}

// darwinDisconnectDeps is Disconnect on macOS: stopWorker boots the
// LaunchAgent out of launchd (success only once the job is no longer loaded)
// and disableAutostart removes its plist, both before the logout and the
// credential removal that disconnectThisPC runs after them. agentErr is a
// failure to build the agent; it fails both steps, so nothing is cleared.
func darwinDisconnectDeps(d disconnectDeps, a *service.LaunchAgent, agentErr error) disconnectDeps {
	d.goos = "darwin"
	// The plist stays while the stop failed: it is what a retry, and a worker
	// that is somehow still running, are tied to.
	stopFailed := false
	d.stopWorker = func() (bool, error) {
		if agentErr != nil {
			stopFailed = true
			return true, agentErr
		}
		err := a.Stop()
		stopFailed = err != nil
		return true, err
	}
	d.disableAutostart = func() (bool, error) {
		if agentErr != nil {
			return true, agentErr
		}
		if stopFailed {
			return true, errors.New("the worker was not stopped, so its LaunchAgent file was kept")
		}
		return true, a.Remove()
	}
	return d
}

var localDisconnectYes bool

var localDisconnectCmd = &cobra.Command{
	Use:   "disconnect",
	Short: "Disconnect this PC from AceTeam (local mode keeps working)",
	Long: `Disconnects this PC from AceTeam: stops the background worker, removes its
sign-in autostart, logs this PC out of the AceTeam Network, and removes the
device key AceTeam gave it. Each step is reported. Local mode keeps working, and local-mode saved logins are never
touched. The setup page's Disconnect this PC button does the same.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runLocalDisconnect(cmd.InOrStdin(), cmd.OutOrStdout(), localDisconnectYes, productionDisconnectDeps())
	},
}

// runLocalDisconnect asks for confirmation (unless yes), runs Disconnect and
// prints each step. It returns an error when a step failed.
func runLocalDisconnect(in io.Reader, out io.Writer, yes bool, d disconnectDeps) error {
	if !yes {
		fmt.Fprint(out, "Disconnect this PC from AceTeam? AceTeam agents will no longer be able to use it. (y/N) ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(out, "Nothing was changed.")
			return nil
		}
	}
	res := disconnectThisPC(d)
	for _, s := range res.Steps {
		fmt.Fprintf(out, "%s: %s. %s\n", s.Name, stepLabel(s.Status), s.Message)
	}
	fmt.Fprintln(out, res.Message)
	if !res.OK {
		return errors.New("disconnect did not finish")
	}
	return nil
}

func stepLabel(status string) string {
	switch status {
	case stepOK:
		return "done"
	case stepNotApplicable:
		return "not needed"
	default:
		return status
	}
}

func init() {
	localDisconnectCmd.Flags().BoolVarP(&localDisconnectYes, "yes", "y", false, "Do not ask for confirmation")
	localCmd.AddCommand(localDisconnectCmd)
}

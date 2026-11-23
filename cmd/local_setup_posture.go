// cmd/local_setup_posture.go
//
// The permission posture of a PC connected to AceTeam from the setup page
// (citadel-cli#1867 C5), and the human-only Files and Shell grant.
//
// writeSetupPosture runs only after a successful setup-page enrollment and
// writes an explicit permissions file: the console, desktop, files and shell
// surfaces off, services, SSH key sync and provisioning off, and remote
// co-browse limited to public addresses (cobrowse_public_only). Co-browse has
// no permission key of its own; it stays available. DefaultPermissions and
// every other enrollment path are unchanged.
//
// setNodeSurface is the one writer for the page's Files and Shell toggles and
// for `citadel permission files|shell`. It loads strictly and refuses a file
// it cannot parse, so a damaged file is never rewritten from the defaults
// (which would turn services, SSH and provisioning back on and drop the
// public-only guard). Each change is logged as one
// `[permission] audit <surface>=<on|off> actor=human via=<surface>` line.
// The toggles are reachable only from the setup page, the local viewer and the
// CLI; no MCP tool and no remote job calls them
// (TestNodePermissionGrantHasNoMCPPath). Remote APPLY_DEVICE_CONFIG keeps its
// existing behavior.
package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/localviewer"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

// setupPostureCopy is shown on the setup page after a connect.
const setupPostureCopy = "AceTeam agents can browse public websites on this PC. They can't open files, run commands, " +
	"use its terminal or screen, or start software on it, and their browser sessions can't reach your home network. " +
	"You can allow files or commands below when a task needs them."

// setupPostureFailedCopy is shown when the connect succeeded but the posture
// could not be saved. The worker is then not offered.
const setupPostureFailedCopy = "Connected, but Citadel could not save this PC's co-browse-only setting, so the worker was not started. " +
	"Check that your user folder is not full or read-only, then press Retry. If it keeps failing, press Disconnect this PC."

// writeSetupPosture writes the co-browse-only posture: the permissions file in
// permissionsDir (with the SetupPosture marker that stops remote config from
// turning anything back on), then meeting.yaml in meetingDir with the meeting
// capability off, because the meeting bot's browser has no egress guard. It
// keeps a passcode hash already in the file and refuses a file it cannot
// read or parse. It is safe to run again: a retry writes the full posture.
func writeSetupPosture(permissionsDir, meetingDir string) error {
	p, err := config.LoadPermissionsStrict(permissionsDir)
	if err != nil {
		return fmt.Errorf("the node permissions file must be readable before the co-browse-only setting is saved: %w", err)
	}
	p.Console, p.Desktop, p.Files, p.Shell = false, false, false, false
	p.Services, p.SSH, p.Provision = false, false, false
	p.CobrowsePublicOnly = true
	p.SetupPosture = config.SetupPostureCobrowseOnly
	if err := config.SavePermissions(permissionsDir, p); err != nil {
		return fmt.Errorf("save the co-browse-only setting: %w", err)
	}
	m := config.LoadMeeting(meetingDir)
	m.MeetingEnabled = false
	if err := config.SaveMeeting(meetingDir, m); err != nil {
		return fmt.Errorf("turn off the meeting capability: %w", err)
	}
	log.Printf("[permission] audit posture=cobrowse_only actor=human via=setup_page")
	return nil
}

// setupPostureQuarantine makes sure a PC whose posture could not be saved
// runs no worker: it removes the sign-in autostart entry and stops the
// per-user background worker, the first two Disconnect steps. Failures are
// logged; the page still withholds the worker and offers Retry.
func setupPostureQuarantine() {
	d := productionDisconnectDeps()
	if _, err := d.disableAutostart(); err != nil {
		log.Printf("[setup] posture not saved: could not remove autostart: %v", err)
	}
	if _, err := d.stopWorker(); err != nil {
		log.Printf("[setup] posture not saved: could not stop the background worker: %v", err)
	}
}

// setupPostureStep names the posture retry's one step.
const setupPostureStep = "Save the co-browse-only setting"

// setupPostureNoRetryCopy refuses a retry that is not pending.
const setupPostureNoRetryCopy = "There is no co-browse-only setting waiting to be saved again on this page."

// setupPostureHolds reports whether permissionsDir still has the posture
// marker and the public-only guard on, read strictly. The setup page starts
// the worker only then.
func setupPostureHolds(permissionsDir string) bool {
	p, err := config.LoadPermissionsStrict(permissionsDir)
	return err == nil && p.CobrowsePublicOnly && p.SetupPosture != ""
}

func init() {
	// Every worker this binary runs (citadel work and the control center)
	// runs only the allowlisted job types on a setup-page PC, read live.
	var cache setupPostureCache
	worker.SetSetupPostureSource(func() bool { return cache.active(nodePermissionsDir()) })
}

// setupPostureCache answers the per-job posture check from the last parse
// while the permissions file's size and modification time are unchanged, so
// a busy fleet worker stats the file per job instead of parsing it.
type setupPostureCache struct {
	mu     sync.Mutex
	dir    string
	key    string
	result bool
}

func (c *setupPostureCache) active(dir string) bool {
	key := "missing"
	if fi, err := os.Stat(filepath.Join(dir, "permissions.yaml")); err == nil {
		key = fmt.Sprintf("%d/%d", fi.Size(), fi.ModTime().UnixNano())
	} else if !errors.Is(err, fs.ErrNotExist) {
		key = ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if key != "" && key == c.key && dir == c.dir {
		return c.result
	}
	c.result = config.SetupPostureActive(dir)
	c.dir, c.key = dir, key
	return c.result
}

// clearSetupPosture removes the SetupPosture marker, loading strictly so a
// damaged file is never rewritten from the defaults. It changes nothing else.
func clearSetupPosture(permissionsDir string) error {
	p, err := config.LoadPermissionsStrict(permissionsDir)
	if err != nil {
		return fmt.Errorf("the node permissions file must be readable before this setting can change: %w", err)
	}
	if p.SetupPosture == "" {
		return nil
	}
	p.SetupPosture = ""
	if err := config.SavePermissions(permissionsDir, p); err != nil {
		return fmt.Errorf("save permissions: %w", err)
	}
	log.Printf("[permission] audit posture=off actor=human via=cli")
	return nil
}

// errUnknownSurface is setNodeSurface's refusal of anything but files/shell.
var errUnknownSurface = errors.New("only files and shell can be changed here")

// setNodeSurface turns Files or Shell on or off in permissionsDir and logs the
// change as an audit line naming via (setup_page, viewer or cli).
func setNodeSurface(permissionsDir, surface string, enabled bool, via string) (*config.Permissions, error) {
	p, err := config.LoadPermissionsStrict(permissionsDir)
	if err != nil {
		return nil, fmt.Errorf("the node permissions file must be readable before this setting can change: %w", err)
	}
	switch surface {
	case localviewer.SurfaceFiles:
		p.Files = enabled
	case localviewer.SurfaceShell:
		p.Shell = enabled
	default:
		return nil, errUnknownSurface
	}
	if err := config.SavePermissions(permissionsDir, p); err != nil {
		return nil, fmt.Errorf("save %s permission: %w", surface, err)
	}
	log.Printf("[permission] audit %s=%s actor=human via=%s", surface, onOff(enabled), via)
	return p, nil
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// Fixed copy for the toggles.
const (
	nodePermsUnavailableCopy = "This PC is not connected to AceTeam, so there is nothing to allow."
	nodePermsUnreadableCopy  = "Citadel could not read this PC's permission settings, so they cannot be changed here. Run \"citadel permission\" in a terminal."
	nodePermsSaveFailedCopy  = "Citadel could not save the change. Check that your user folder is not full or read-only, then try again."
	nodePermsBusyCopy        = "Wait for connecting or disconnecting to finish, then try again."
	nodePermsShellLockedCopy = "Commands stay refused until this PC has a passcode. To set one, run \"citadel passcode set\" in a terminal."
)

// nodePermissionDeps are what the toggles read and change, as seams.
type nodePermissionDeps struct {
	// dir resolves the node config dir at click time.
	dir func() string
	// connected reports whether this PC has an AceTeam registration.
	connected func() bool
	// workspace names the folder Files covers.
	workspace func() string
	// busy refuses a change while an enrollment or disconnect runs.
	busy func() bool
	// via names the page in the audit line.
	via string
}

// nodePermissionState reads the toggles' state. Every error is fixed copy.
func nodePermissionState(d nodePermissionDeps) (localviewer.NodePermissionState, error) {
	if d.connected == nil || !d.connected() {
		return localviewer.NodePermissionState{Message: nodePermsUnavailableCopy}, nil
	}
	p, err := config.LoadPermissionsStrict(d.dir())
	if err != nil {
		return localviewer.NodePermissionState{Message: nodePermsUnreadableCopy}, nil
	}
	st := localviewer.NodePermissionState{
		Available:  true,
		Files:      p.Files,
		Shell:      p.Shell,
		Console:    p.Console,
		Desktop:    p.Desktop,
		Passcode:   p.HasPasscode(),
		PublicOnly: p.CobrowsePublicOnly,
	}
	if d.workspace != nil {
		st.Workspace = d.workspace()
	}
	if st.Shell && !st.Passcode {
		st.Message = nodePermsShellLockedCopy
	}
	return st, nil
}

// newNodePermissionHooks builds the page hooks.
func newNodePermissionHooks(d nodePermissionDeps) *localviewer.NodePermissionHooks {
	return &localviewer.NodePermissionHooks{
		Get: func() (localviewer.NodePermissionState, error) { return nodePermissionState(d) },
		Set: func(surface string, enabled bool) (localviewer.NodePermissionState, error) {
			if d.busy != nil && d.busy() {
				st, _ := nodePermissionState(d)
				return st, errors.New(nodePermsBusyCopy)
			}
			st, _ := nodePermissionState(d)
			if !st.Available {
				if st.Message == "" {
					st.Message = nodePermsUnavailableCopy
				}
				return st, errors.New(st.Message)
			}
			if _, err := setNodeSurface(d.dir(), surface, enabled, d.via); err != nil {
				log.Printf("[permission] %s change refused: %v", surface, err)
				if errors.Is(err, errUnknownSurface) {
					return st, errUnknownSurface
				}
				return st, errors.New(nodePermsSaveFailedCopy)
			}
			return nodePermissionState(d)
		},
	}
}

// productionNodePermissionDeps reads this machine's node config dir at click
// time; via names the page.
func productionNodePermissionDeps(via string, busy func() bool) nodePermissionDeps {
	return nodePermissionDeps{
		dir:       func() string { return nodeConfigDirFn() },
		connected: hasNodeRegistration,
		workspace: workspaceDirPath,
		busy:      busy,
		via:       via,
	}
}

// viewerNodePermissions is the local viewer's Files and Shell toggles. Nothing
// is resolved until the viewer page asks.
func viewerNodePermissions() *localviewer.NodePermissionHooks {
	return newNodePermissionHooks(productionNodePermissionDeps("viewer", nil))
}

package jobs

import (
	"fmt"

	citadelconfig "github.com/aceteam-ai/citadel-cli/internal/config"
)

// setupPostureRemoteCopy explains a refusal on a PC that keeps its own
// permission settings (citadel-cli#1867 C5).
const setupPostureRemoteCopy = "this PC was connected from its setup page, so only the person at the PC can turn it on (the setup page, the viewer, or 'citadel permission' on the PC)"

// applySetupPostureFilter is the remote side of a setup-page PC's posture
// (citadel-cli#1867 C5). When permissionsDir carries a SetupPosture it removes
// from config every request that would turn something on or push a secret,
// and returns one job-result line per refusal. Requests that turn something
// off are left alone. On every other node it returns "" and leaves config
// unchanged.
//
// Refused while the posture is set: enabling Console, Desktop, Files or Shell;
// any node passcode push; enabling the meeting capability, the egress relay or
// its LAN destinations, default-serve or VNC; starting services; and adding
// services to the manifest (which a worker start would bring up). When the
// permissions file cannot be parsed, every permission field is dropped so the
// lenient save below never rewrites it from the defaults.
func applySetupPostureFilter(permissionsDir string, config *DeviceConfig) string {
	if !citadelconfig.SetupPostureActive(permissionsDir) {
		return ""
	}
	var result string
	refuse := func(what string) {
		result += fmt.Sprintf("\nRefused: %s cannot be turned on remotely: %s", what, setupPostureRemoteCopy)
	}
	refuseOn := func(p **bool, what string) {
		if *p != nil && **p {
			*p = nil
			refuse(what)
		}
	}
	if _, err := citadelconfig.LoadPermissionsStrict(permissionsDir); err != nil {
		if config.ConsoleEnabled != nil || config.DesktopEnabled != nil || config.FilesEnabled != nil || config.ShellEnabled != nil || config.NodePasscode != nil {
			config.ConsoleEnabled, config.DesktopEnabled, config.FilesEnabled, config.ShellEnabled, config.NodePasscode = nil, nil, nil, nil, nil
			result += "\nRefused: permission changes ignored, this PC's permissions file could not be read; fix it on the PC"
		}
	}
	refuseOn(&config.ConsoleEnabled, "Console (terminal) access")
	refuseOn(&config.DesktopEnabled, "Desktop (screen/VNC) access")
	refuseOn(&config.FilesEnabled, "Files access")
	refuseOn(&config.ShellEnabled, "Shell (remote command) access")
	if config.NodePasscode != nil {
		config.NodePasscode = nil
		result += "\nRefused: the node passcode cannot be set remotely: " + setupPostureRemoteCopy
	}
	refuseOn(&config.MeetingEnabled, "the meeting capability")
	refuseOn(&config.EgressRelay, "the egress relay")
	refuseOn(&config.EgressAllowLan, "egress relay LAN destinations")
	refuseOn(&config.DefaultServe, "default-serve")
	if config.VNCEnabled {
		config.VNCEnabled = false
		refuse("the VNC server")
	}
	if len(config.Services) > 0 || config.AutoStartServices {
		config.Services, config.AutoStartServices = nil, false
		refuse("services")
	}
	return result
}

// cmd/init_service.go
//
// `citadel init`'s post-enroll worker-setup hooks, so a single `citadel init`
// leaves a serving node instead of a half-enrolled one that is on the mesh but
// runs no worker.
//
//   - macOS (maybeInstallDarwinNodeService, citadel-cli#1043): installs and
//     starts the node worker as a managed launchd service.
//   - Linux (maybeFinishLinuxNodeSetup, citadel-cli#1080): scaffolds
//     node_config_dir + a minimal manifest (so whoami/status resolve) and
//     enables/starts the worker unit. maybeFinishLinuxNodeSetup owns the
//     root-vs-non-root decision; see its doc comment.
//   - Windows: no init-driven service install today.
package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/service"
)

// maybeInstallDarwinNodeService installs and (re)starts the launchd node
// service on macOS. It is a no-op on other platforms and is deliberately
// conservative about WHEN it installs, so a `KeepAlive` service can never be
// left crash-looping:
//
//   - Skips unless the node has device credentials configured
//     (hasDeviceConfigured) -- a `citadel work` with no creds would exit
//     immediately and launchd would relaunch it every ~10s forever.
//   - Skips when the operator chose to skip the network (NetChoiceSkip).
//
// Root vs non-root decides the service form (advisor review, citadel-cli#1043):
//   - non-root (the common no-sudo `citadel init`): a user LaunchAgent
//     (~/Library/LaunchAgents, domain gui/<uid>) -- starts at LOGIN.
//   - root (e.g. `sudo citadel init --provision`): a system LaunchDaemon
//     (/Library/LaunchDaemons, domain system) -- starts at BOOT without login.
//
// A user LaunchAgent's "survives a reboot" means "re-registers and starts at
// the next login"; true boot-time-without-login requires the daemon form.
func maybeInstallDarwinNodeService(choice nexus.NetworkChoice) {
	if !platform.IsDarwin() {
		return
	}
	if choice == nexus.NetChoiceSkip {
		return
	}
	if !hasDeviceConfigured() {
		// No worker credentials: installing a KeepAlive `citadel work` here
		// would crash-loop. Leave the service uninstalled; the operator can run
		// `citadel login` then `citadel service install` once configured.
		Debug("darwin: skipping launchd service install (no device credentials configured)")
		return
	}

	cfg, err := service.DefaultConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Could not resolve service config for launchd install: %v\n", err)
		return
	}
	// DefaultConfig adds the managed-helper update guard for the desktop app.
	// Root -> system LaunchDaemon (boot-time); non-root -> user LaunchAgent
	// (login-time). Never a gui/0 LaunchAgent for root (that GUI domain doesn't
	// exist).
	cfg.UserMode = !platform.IsRoot()

	mgr := service.NewManager()
	if err := mgr.Install(cfg); err != nil {
		// The plist is written before the launchctl (re)load is attempted, so a
		// load failure here (e.g. bootstrap into gui/<uid> from an SSH session
		// with no Aqua session) still leaves a RunAtLoad service that launchd
		// will start at the next login/reboot -- it just isn't loaded right now.
		fmt.Fprintf(os.Stderr, "⚠️  Installed the launchd service but could not start it now: %v\n", err)
		fmt.Fprintln(os.Stderr, "   It will start automatically at the next login/reboot.")
		fmt.Fprintln(os.Stderr, "   To start it now, run: citadel service start")
		return
	}
	if cfg.UserMode {
		fmt.Println("\n✅ Installed the Citadel launchd service (starts at login, survives reboot).")
	} else {
		fmt.Println("\n✅ Installed the Citadel launchd daemon (starts at boot, survives reboot).")
	}
}

// maybeFinishLinuxNodeSetup is `citadel init`'s Linux counterpart to
// maybeInstallDarwinNodeService (citadel-cli#1080): after a successful enroll it
// (1) scaffolds node_config_dir + a minimal manifest so `citadel whoami` /
// `citadel status` stop reporting a missing node_config_dir / manifest, and
// (2) enables/starts the node worker so a single `citadel init` yields a
// serving node. No-op off Linux.
//
// It uses the SAME guards as the darwin hook -- skip when the network was
// skipped or no device credentials are configured -- so it never leaves a
// Restart=always worker crash-looping with no creds. This is ALSO why it never
// fires inside install.sh: install.sh runs `citadel init --authkey`, which
// joins the mesh but writes NO device credentials (saveDeviceConfigToFile is
// reached only on the device-auth path), so hasDeviceConfigured() is false and
// install.sh keeps sole ownership of citadel-worker.service -- there is no
// competing-unit race. The hook fires on the interactive device-auth enroll
// (the RM-01/Jetson demo path), where creds are present.
//
// Root vs non-root decides how the worker is started (decideLinuxWorkerSetup):
//   - root, an existing citadel-managed unit on disk (install.sh/packer's
//     citadel-worker.service, or a prior `citadel service install`):
//     `systemctl enable --now` it (idempotent -- never restarts a running
//     worker, so no dropped jobs).
//   - root, no unit yet (standalone `citadel init`): install a fresh managed
//     system unit via service.Manager, refusing if a competing active unit
//     exists (same guard `citadel service install` uses).
//   - non-root (cannot manage a system unit): scaffold what is safe and print
//     the EXACT next command to run, never a silent half-enrollment.
func maybeFinishLinuxNodeSetup(choice nexus.NetworkChoice, nodeName string) {
	if !platform.IsLinux() {
		return
	}
	if choice == nexus.NetChoiceSkip {
		return
	}
	if !hasDeviceConfigured() {
		Debug("linux: skipping worker setup (no device credentials configured)")
		return
	}

	// Part 1: scaffold node_config_dir + a minimal manifest (safe root or not),
	// so findAndReadManifest resolves and whoami/status stop warning.
	if err := ensureNodeScaffold(nodeConfigDirFn(), nodeName); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Could not scaffold node configuration: %v\n", err)
	}

	// Part 2: start/enable the worker (or name the next command when non-root).
	// A root --provision invocation from a normal account installs that
	// account's lingered user unit, matching its rootless Podman socket.
	if finishProvisionedLinuxUserWorker() {
		return
	}
	applyLinuxWorkerSetup(decideLinuxWorkerSetup(platform.IsRoot(), service.InstalledManagedUnit))
}

// linuxWorkerAction enumerates how `citadel init` finishes worker setup on
// Linux.
type linuxWorkerAction int

const (
	linuxWorkerNoop           linuxWorkerAction = iota
	linuxWorkerEnableExisting                   // root: `enable --now` an already-installed unit
	linuxWorkerInstallNew                       // root: install a fresh managed system unit
	linuxWorkerPrintCommand                     // non-root: print the exact next command
)

// linuxWorkerDecision is the outcome of decideLinuxWorkerSetup: which action to
// take, the unit it applies to (for the enable/print cases), and the exact next
// command to print (for the non-root case).
type linuxWorkerDecision struct {
	action      linuxWorkerAction
	unit        service.ManagedUnit
	unitFound   bool
	nextCommand string
}

// decideLinuxWorkerSetup is the pure decision for how `citadel init` finishes
// worker setup on Linux, given whether the invocation is root and which
// citadel-managed unit (if any) is already installed on disk. Kept pure +
// injectable (installedUnit) so the branch selection is unit-testable without
// touching systemctl or the filesystem, mirroring competingManagedUnit.
func decideLinuxWorkerSetup(isRoot bool, installedUnit func() (service.ManagedUnit, bool)) linuxWorkerDecision {
	unit, found := installedUnit()
	if isRoot {
		if found {
			return linuxWorkerDecision{action: linuxWorkerEnableExisting, unit: unit, unitFound: true}
		}
		return linuxWorkerDecision{action: linuxWorkerInstallNew}
	}
	// Non-root cannot manage a system unit; name the exact next command instead
	// of failing or silently half-enrolling. An installed unit gets its own
	// enable command (which is sudo-free for a user unit); otherwise point at a
	// system-service install.
	if found {
		return linuxWorkerDecision{
			action:      linuxWorkerPrintCommand,
			unit:        unit,
			unitFound:   true,
			nextCommand: unit.EnableNowCommand(),
		}
	}
	return linuxWorkerDecision{
		action:      linuxWorkerPrintCommand,
		nextCommand: "sudo citadel service install --system",
	}
}

// applyLinuxWorkerSetup executes a linuxWorkerDecision. The side-effectful glue
// is kept thin (the testable logic lives in decideLinuxWorkerSetup +
// ensureNodeScaffold); it is inspection-verified like maybeInstallDarwinNodeService.
func applyLinuxWorkerSetup(d linuxWorkerDecision) {
	switch d.action {
	case linuxWorkerEnableExisting:
		if err := d.unit.EnableNow(); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Could not start the worker service now: %v\n", err)
			fmt.Fprintf(os.Stderr, "   Run: %s\n", d.unit.EnableNowCommand())
			return
		}
		fmt.Printf("\n✅ Worker service enabled and started (%s).\n", d.unit.Description())
	case linuxWorkerInstallNew:
		installLinuxWorkerService()
	case linuxWorkerPrintCommand:
		fmt.Println("\n✅ Node enrolled. To finish setup and start the worker, run:")
		fmt.Printf("     %s\n", d.nextCommand)
	case linuxWorkerNoop:
		// nothing to do
	}
}

// installLinuxWorkerService installs+starts a fresh managed system worker unit
// via service.Manager, refusing if a competing active citadel unit already
// exists (the same guard `citadel service install` uses). Root-only path.
func installLinuxWorkerService() {
	cfg, err := service.DefaultConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Could not resolve worker service config: %v\n", err)
		fmt.Fprintln(os.Stderr, "   Install it later with: sudo citadel service install --system")
		return
	}
	cfg.Args = []string{"work"}
	cfg.UserMode = false // root -> system unit (starts at boot, no login required)

	if unit, competing := competingManagedUnit(cfg, activeManagedUnitFn); competing {
		fmt.Fprintln(os.Stderr, competingManagedUnitError(unit).Error())
		return
	}

	if err := newServiceManagerFn().Install(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Installed the worker service but could not start it now: %v\n", err)
		fmt.Fprintln(os.Stderr, "   Start it later with: sudo systemctl start citadel")
		return
	}
	fmt.Println("\n✅ Installed and started the Citadel worker service (starts on boot).")
}

// ensureNodeScaffold makes findAndReadManifest resolve after a network-only
// enroll so `citadel whoami`/`citadel status` stop reporting a missing
// node_config_dir / manifest (citadel-cli#1080). Production wrapper; resolves
// platform.ConfigDir() and delegates to the pure core.
func ensureNodeScaffold(nodeConfigDir, nodeName string) error {
	return ensureNodeScaffoldAt(nodeConfigDir, nodeName, filepath.Join(platform.ConfigDir(), "config.yaml"))
}

// ensureNodeScaffoldAt is the pure, path-parameterized core of
// ensureNodeScaffold (globalConfigFile is passed in, not resolved via
// platform.ConfigDir(), so a test can point it at a temp file -- see the
// CLAUDE.md ConfigDir()/GetNodeConfigDir() note on why a test must never write
// through the real resolved paths on a machine that runs a live node).
//
// It reuses the existing writers: a minimal citadel.yaml (only when ABSENT --
// never clobbering a provisioned manifest) at the machine-convergent node
// config dir, plus the merge-preserving global node_config_dir pointer.
// Idempotent.
func ensureNodeScaffoldAt(nodeConfigDir, nodeName, globalConfigFile string) error {
	if nodeConfigDir == "" {
		return fmt.Errorf("no node config dir resolved")
	}
	if nodeName == "" {
		if h, _ := os.Hostname(); h != "" {
			nodeName = h
		} else {
			nodeName = "citadel-node"
		}
	}

	if err := os.MkdirAll(filepath.Join(nodeConfigDir, "services"), 0755); err != nil {
		return fmt.Errorf("create node config dir %s: %w", nodeConfigDir, err)
	}

	// Write a minimal manifest only when none exists, so a provisioned
	// citadel.yaml (or one from a prior run) is never overwritten.
	manifestPath := filepath.Join(nodeConfigDir, "citadel.yaml")
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		manifest := &CitadelManifest{
			Node: struct {
				Name  string   `yaml:"name"`
				Tags  []string `yaml:"tags"`
				OrgID string   `yaml:"org_id,omitempty"`
			}{
				Name: nodeName,
				Tags: []string{},
			},
			Services: []Service{},
		}
		if err := writeManifest(manifestPath, manifest); err != nil {
			return err
		}
	}

	return writeGlobalConfigFile(globalConfigFile, nodeConfigDir)
}

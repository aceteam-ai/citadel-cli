// cmd/nodedir.go
//
// --node-dir / CITADEL_NODE_DIR (citadel#853) exists because of a real
// production incident: a subagent smoke-testing `citadel module` had set an
// isolated $HOME for one shell call, but a LATER call in the same session ran
// without it (shell state does not persist between an agent's tool calls), so
// `citadel module stop/restart` fell through to the default $HOME/ConfigDir
// resolution and cycled a LIVE production container.
//
// findAndReadManifest/findOrCreateManifest (cmd/manifest.go) are the single
// choke point almost every manifest-mutating command in this package goes
// through (module stop/start/restart, run, stop, catalog/module install,
// services, status, ...). Threading the override in there, rather than at each
// call site, is what makes it consistently honored: any command that reads the
// node manifest through those two functions gets --node-dir for free, with no
// per-command checklist to keep in sync.
//
// SCOPE: this override redirects citadel.yaml + services/ dir resolution ONLY.
// It does NOT redirect network state (internal/network.GetStateDir /
// GetNodeConfigDir -- the tsnet mesh identity), the module lockfile
// (catalog.LockfilePath, still platform.ConfigDir()), or anything under
// nodevault/worklock. A caller reaching for "a fully isolated node" needs more
// than this flag. Two places actively guard the resulting manifest/lockfile
// split rather than just documenting it:
//   - refuseIfLockfileWriteUnsupported below, called from the three CLI
//     install/update entry points (a one-shot command: an operator error is
//     the right response).
//   - runWork's boot-time refusal (cmd/work.go): the reconcile loop's and
//     MODULE_SET's install/uninstall run inside a long-lived process, so a
//     per-call refusal there would just be an infinite quiet converge
//     failure -- refuse ONCE, loudly, at startup instead.
package cmd

import (
	"fmt"
	"os"
	"strings"
)

// nodeDirFlag is the value of the global --node-dir flag (empty if unset).
var nodeDirFlag string

// resolveNodeDirOverride returns the operator-supplied node directory
// override, if any: --node-dir wins over CITADEL_NODE_DIR. Empty means "no
// override" -- the normal $HOME/ConfigDir resolution in
// findAndReadManifest/findOrCreateManifest applies completely unchanged, so a
// node that never sets either is byte-for-byte the pre-#853 behavior.
func resolveNodeDirOverride() string {
	if v := strings.TrimSpace(nodeDirFlag); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("CITADEL_NODE_DIR"))
}

func init() {
	rootCmd.PersistentFlags().StringVar(&nodeDirFlag, "node-dir", "", "Override the node config directory (citadel.yaml + services/), bypassing $HOME/ConfigDir resolution. Also settable via CITADEL_NODE_DIR. Scope: manifest/services resolution only -- does NOT redirect network state, the module lockfile, or nodevault.")
}

// refuseIfLockfileWriteUnsupported guards the commands whose install/update
// path writes BOTH the node manifest (which resolveNodeDirOverride now
// redirects, via findAndReadManifest/findOrCreateManifest) AND the module
// lockfile (catalog.LockfilePath/UpsertLockEntry/DeleteLockEntry, which is
// hardcoded to platform.ConfigDir() and does NOT consult the override).
//
// Half-honoring an override is worse than not honoring it at all: without this
// guard, `citadel module install <src> --node-dir /tmp/x` would register the
// service in /tmp/x/citadel.yaml while writing its provenance into the REAL
// machine's modules.lock -- a manifest/lockfile split that leaves the node in
// a state nothing else in this codebase expects (ListInstalled scoping,
// citadel#739, assumes the two always agree). Refusing loudly is safer than
// silently producing that split; threading the override into the catalog
// lockfile path is deferred, see the PR for the tracking issue.
//
// Not needed for `citadel module stop|start|restart` (cmd/module_control.go):
// those only touch the desired_status marker + compose up/down, never the
// lockfile.
func refuseIfLockfileWriteUnsupported(cmdLabel string) error {
	override := resolveNodeDirOverride()
	if override == "" {
		return nil
	}
	return fmt.Errorf(
		"%s does not yet support --node-dir/CITADEL_NODE_DIR: it writes to the module lockfile "+
			"(modules.lock), which is not override-aware and would still resolve to this machine's "+
			"real config directory while the manifest write went to %q -- a split state this codebase "+
			"does not expect. Unset the override to run this command, or use 'citadel module "+
			"stop|start|restart' (which IS override-aware end to end) for targeted node operations.",
		cmdLabel, override)
}

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
// SCOPE (read this before assuming --node-dir gives you an isolated node --
// citadel#856 review of the original #853/#854 PR corrected an overclaim
// here): the override redirects citadel.yaml + services/ dir RESOLUTION,
// the compose PROJECT identity used for invocations driven through that
// resolved manifest (composeProjectOverride/composeArgsWithProject below),
// and, for EMBEDDED services only (services.ServiceMap; citadel#860,
// embeddedContainerName below), the container_name each one's compose file
// pins. It does NOT redirect network state (internal/network.GetStateDir/
// GetNodeConfigDir -- the tsnet mesh identity), the module lockfile
// (catalog.LockfilePath, still platform.ConfigDir()), anything under
// nodevault/worklock, or catalog/third-party module container_name (those are
// author-controlled, not templated by citadel -- see embeddedContainerName's
// SCOPE note).
//
// History: every embedded compose file pins a GLOBAL `container_name:
// citadel-<svc>` (services/compose/*.yml). Before #860, that was unchanged by
// which directory citadel materialized/read the compose file from -- on a
// machine whose Docker daemon ALSO runs a real citadel node (the exact
// production topology this flag was built to be used safely against),
// `citadel run vllm --node-dir /tmp/x` materialized a vllm.yml in /tmp/x
// naming the SAME `citadel-vllm` container the real node manages. #856's
// compose-project scoping closed the compose-invocation half of that gap
// (down -> safe no-op, up -> loud cross-project failure instead of a silent
// mutation) but did not namespace container_name itself, so two DIFFERENT
// override dirs both materializing the same embedded service still collided
// with EACH OTHER. #860 (embeddedContainerName, cmd/manifest.go's
// ensureComposeFile, internal/jobs/service_handler.go's
// ensureEmbeddedComposeFile) closes that: under an active override, the
// materialized container_name is namespaced to "citadel-<hash>-<svc>", <hash>
// matching the compose "-p" project's hash so the two never disagree.
//
// The one guarantee that does NOT depend on any of the above:
// --expect-node (cmd/module_control.go) fails closed on a resolved-identity
// mismatch regardless of what the Docker layer would have done -- if you need
// an actual cross-node safety net rather than "a friendlier failure mode",
// that flag is it, not --node-dir alone.
//
// Two places actively guard the manifest/lockfile split (a DIFFERENT gap,
// see refuseIfLockfileWriteUnsupported) rather than just documenting it:
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

	"github.com/aceteam-ai/citadel-cli/internal/compose"
	"github.com/aceteam-ai/citadel-cli/services"
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

// refuseIfReservationNodeDirUnsupported guards the model-exclusivity paths
// (`citadel run --exclusive`, `citadel module reservations *`, and the local
// MCP local_model_deploy/local_run_exclusive/local_model_stop tools,
// aceteam#8248/#8249) that call straight into internal/jobs.ServiceHandler's
// Reserve/ReserveExclusive/Release/StartServiceWithModel.
//
// Those resolve citadel.yaml from the caller-supplied configDir (itself
// --node-dir-aware, since it flows through findAndReadManifest/
// findOrCreateManifest) -- but internal/jobs.ensureEmbeddedComposeFile's own
// container-name-namespacing reconciliation (citadel#860) reads
// CITADEL_NODE_DIR directly from the ENVIRONMENT, not the --node-dir cobra
// flag, because internal/jobs cannot see cobra flags (only cmd/ wires them).
// A `--node-dir /tmp/x` invocation using the FLAG form would therefore
// materialize/start services under the UNNAMESPACED "citadel-<svc>"
// container_name while durably evicting/starting the REAL node's services --
// exactly the citadel#853/#856/#860 incident class this package exists to
// prevent. Refusing loudly beats silently producing that split.
//
// Using CITADEL_NODE_DIR (the env var) instead of --node-dir (the flag) is
// NOT refused: internal/jobs sees that value directly, so there is no
// flag/env divergence to guard against in that case.
func refuseIfReservationNodeDirUnsupported(cmdLabel string) error {
	override := resolveNodeDirOverride()
	if override == "" {
		return nil
	}
	if strings.TrimSpace(os.Getenv("CITADEL_NODE_DIR")) == override {
		return nil
	}
	return fmt.Errorf(
		"%s does not yet support --node-dir as a FLAG: it calls internal/jobs.ServiceHandler "+
			"directly, which only sees CITADEL_NODE_DIR via the environment, not this flag -- "+
			"container-name namespacing (citadel#860) would silently NOT apply while eviction/"+
			"start actions still target %q. Set CITADEL_NODE_DIR=%q in the environment instead of "+
			"--node-dir to run this command, or unset the override to run it against this machine's "+
			"real node",
		cmdLabel, override, override)
}

// composeProjectOverride returns the compose "-p"/"--project-name" value to
// use for compose invocations while --node-dir/CITADEL_NODE_DIR is active, or
// "" when no override is set. "" is the signal every call site below uses to
// mean "apply no -p at all" -- the #528-mandated default (compose derives the
// project from the compose file's parent directory basename, "services"),
// left EXACTLY as-is when no override is active.
//
// WHY THIS EXISTS (citadel#856 review): see this file's package doc comment
// for the full incident. Short version: --node-dir does not, by itself, give
// a compose action a different Docker container identity than the real
// node's, because every embedded compose file pins a global container_name.
// Deriving a project name from the override directory and passing it as -p
// makes compose select/manage containers by the com.docker.compose.project
// label scoped to THIS override, so a `down` against the override's compose
// file cannot match the real node's container (wrong label -> no-op) and an
// `up` against it fails loudly on the container_name conflict instead of
// silently reusing/destroying the real container.
//
// Deterministic (sha256 of the absolute, cleaned override path) rather than
// e.g. a random suffix, so repeated invocations against the SAME override dir
// converge on the SAME project (compose's own idempotency -- "is my container
// already up" -- depends on this), while two DIFFERENT override dirs get
// DIFFERENT projects. Truncated to 12 hex chars: short enough to stay well
// under Docker's 63-byte project/label-derived name limits even after
// compose's own "citadel-nodedir-<hash>_<service>_1"-style container naming
// (moot here since container_name is pinned, but the project name itself
// also feeds network/volume names compose creates), long enough that an
// accidental collision between two override dirs is not a practical concern.
func composeProjectOverride() string {
	hash := compose.NodeDirHash(resolveNodeDirOverride())
	if hash == "" {
		return ""
	}
	return "citadel-nodedir-" + hash
}

// composeArgsWithProject prepends "-p <project>" to a compose invocation's
// args when --node-dir/CITADEL_NODE_DIR is active, and returns args
// COMPLETELY UNCHANGED otherwise -- preserving the #528 no-`-p` default
// exactly (that is what backward compatibility means here: byte-identical
// argv when the override is unset). "-p"/"-f" and other compose GLOBAL flags
// must precede the subcommand (up/down/restart/ps) per the docker/podman
// compose CLI, so this must be applied to the args slice BEFORE the
// subcommand and its own flags are appended -- every call site below follows
// that order; see each one's inline comment for the exact argv shape.
//
// Every compose invocation this package drives against a citadel-owned
// compose file (up, down, restart, ps) MUST route through this, or the
// override-dir project scoping in composeProjectOverride's doc comment does
// not apply uniformly and a caller could believe --node-dir is safe against a
// call site that silently isn't. See composeCommandFor (cmd/service.go,
// covers ps + restart), startServiceComposeArgs (cmd/service.go, covers up),
// stopComposeArgs (cmd/stop.go, covers down).
func composeArgsWithProject(args []string) []string {
	project := composeProjectOverride()
	if project == "" {
		return args
	}
	out := make([]string, 0, len(args)+2)
	out = append(out, "-p", project)
	return append(out, args...)
}

// embeddedContainerName returns the container name to materialize/expect for
// an EMBEDDED service (a services.ServiceMap entry) named svc: unchanged
// "citadel-<svc>" with no --node-dir/CITADEL_NODE_DIR override active (byte-
// identical to pre-#860 behavior), or the override-namespaced
// "citadel-<hash>-<svc>" when one is (citadel#860, closing the gap
// composeProjectOverride's doc comment above describes -- the compose "-p"
// project scoping alone does not stop two DIFFERENT override dirs from both
// materializing a `container_name: citadel-vllm` and colliding with EACH
// OTHER). <hash> is compose.NodeDirHash(resolveNodeDirOverride()) -- the SAME
// value composeProjectOverride derives for "-p", so a compose action's project
// and the container it starts under always agree on which override owns them;
// see internal/compose/nodedir.go's package doc for why that sharing matters
// and lives there (both cmd and internal/jobs need to reach it, and jobs
// cannot import cmd).
//
// This is the single choke point materialization (cmd.ensureComposeFile) and
// the resolvers (containerIsRunning, dryRunContainerNames) all route through,
// so they cannot disagree on what an embedded service's container is named.
//
// SCOPE: for services.ServiceMap entries only -- do not call this for a
// catalog/third-party module name (those author their own container_name and
// are not namespaced by --node-dir; see cmd/module_ops.go's containerIsRunning
// for the ServiceMap-membership gate callers must apply first).
func embeddedContainerName(svc string) string {
	return compose.ContainerName(svc, resolveNodeDirOverride())
}

// isEmbeddedService reports whether name is a services.ServiceMap entry --
// the ServiceMap-membership gate embeddedContainerName's callers must apply
// (see its doc comment): namespacing container_name under --node-dir is
// scoped to EMBEDDED services only (citadel#860's non-goal), never to
// catalog/third-party modules, which author their own container_name.
func isEmbeddedService(name string) bool {
	_, ok := services.ServiceMap[name]
	return ok
}

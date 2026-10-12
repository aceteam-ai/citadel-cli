// cmd/roots.go
//
// Machine-convergent resolution of the authorized-roots allowlist (aceteam#10876
// C6, citadel-cli#1273). The allowlist is the directories a dispatched read job
// (FILE_INDEX first) may read outside the workspace, AND the directories the
// local `citadel search` surface + the on-node file watcher index.
//
// It MUST be machine-convergent: a root-owned `citadel work` reads its roots
// from network.GetNodeConfigDir(), while an operator binds a root with
// `citadel search roots add` from their own shell. If those diverged (the
// ConfigDir() invoker-scoping landmine in CLAUDE.md), the dispatched FILE_INDEX
// would never see the bound root and the refusal's remediation message would be
// a lie. So both read AND write go through the SAME dirs here: the convergent
// node config dir is primary, platform.ConfigDir() is a legacy read fallback for
// roots bound by a pre-#1273 binary. Writes always land in the primary dir.
//
// The dir resolution lives in cmd (which may call network.GetNodeConfigDir());
// the pure read lives in config.LoadRootsFrom (a leaf, testable with t.TempDir()).
package cmd

import (
	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// rootsConfigDirs returns the ordered config dirs for the authorized-roots
// allowlist: the machine-convergent node config dir first (so a dispatched root
// worker and an operator CLI agree), then the legacy invoker-scoped
// platform.ConfigDir() as a read fallback. dirs[0] is the write target.
func rootsConfigDirs() []string {
	return []string{network.GetNodeConfigDir(), platform.ConfigDir()}
}

// rootsWriteDir returns the single dir authorized-roots mutations
// (add/remove/save) write to — the first non-empty entry of rootsConfigDirs,
// i.e. the machine-convergent node config dir.
func rootsWriteDir() string {
	for _, d := range rootsConfigDirs() {
		if d != "" {
			return d
		}
	}
	return platform.ConfigDir()
}

// loadConvergedRoots reads the authorized-roots allowlist from the convergent
// dirs (primary, then legacy fallback). It is the single read path every roots
// consumer in cmd uses so the dispatched handlers, the watcher, and the search
// CLI never disagree about which roots exist.
func loadConvergedRoots() *config.Roots {
	return config.LoadRootsFrom(rootsConfigDirs()...)
}

// authorizedRootsResolver returns a live resolver for the dispatched read
// handlers (aceteam#10876 C6). The config DIRS are resolved ONCE (here, at worker
// boot) — a node's config dir does not move during a worker's lifetime, and
// network.GetNodeConfigDir() is not free (it reads /etc/citadel/config.yaml,
// then an owner-home fallback) so it must not run on every dispatch. Only the
// cheap roots.yaml READ is live: a root bound with `citadel search roots add`
// is honored on the next dispatch without a restart — exactly what the refusal's
// remediation message promises. An empty result means workspace-only.
func authorizedRootsResolver() func() []string {
	dirs := rootsConfigDirs()
	return func() []string {
		return config.LoadRootsFrom(dirs...).Roots
	}
}

//go:build darwin

package service

import (
	"os"

	"github.com/aceteam-ai/citadel-cli/internal/update"
)

// RematerializeManagedUnits re-renders the citadel launchd plist(s) on disk so a
// binary upgrade reaches an already-installed macOS node, mirroring the
// systemd path's intent (citadel-cli#1043). The macOS-specific drift it heals:
// an OLDER binary baked a VERSIONED Homebrew Cellar path
// (<prefix>/Cellar/citadel/<ver>/bin/citadel) into ProgramArguments; the next
// `brew upgrade` removes that Cellar version, leaving the LaunchAgent/Daemon
// pointing at a path that no longer exists (a service that silently fails to
// start on the next reboot). This rewrites the ExecPath to the STABLE
// <prefix>/bin/citadel symlink that survives `brew upgrade`.
//
// It is FILE-ONLY: it never calls launchctl (no bootout/bootstrap). Two
// reasons, matching the Linux contract: (1) this runs at worker boot
// (cmd/work.go) as well as on `citadel update install`, and a bootout from
// inside the running worker would kill the very process calling this and loop;
// (2) the update flow restarts the service on its own terms (mgr.Stop+Start),
// at which point the rewritten plist is re-read. A plist that needs no healing
// is left byte-for-byte untouched, so this triggers no churn on the common
// (non-Homebrew) node. logf may be nil.
func RematerializeManagedUnits(logf func(format string, args ...any)) ([]string, error) {
	log := func(format string, args ...any) {
		if logf != nil {
			logf(format, args...)
		}
	}

	var rewritten []string
	// User agent first, then system daemon (mirrors detectInstalledMode's
	// user-first preference and candidateManagedUnits' order).
	for _, userMode := range []bool{true, false} {
		pp, err := plistPath(userMode)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(pp)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			log("plist-refresh: %s: read failed, skipping: %v", pp, err)
			continue
		}
		content := string(data)

		if !isCitadelManagedPlist(content) {
			log("plist-refresh: %s: not a citadel-managed plist; leaving untouched", pp)
			continue
		}

		args := parseLaunchdProgramArguments(content)
		if len(args) == 0 {
			continue
		}
		stable, isBrew := update.StableDarwinExecPath(args[0])
		if !isBrew || stable == args[0] {
			// Not a Homebrew-Cellar ExecPath, or already the stable path:
			// nothing to heal (no write, no churn).
			continue
		}

		newContent, changed := replaceFirstProgramArgument(content, stable)
		if !changed {
			continue
		}

		// The system LaunchDaemon lives under /Library and requires root to
		// rewrite. Skip (with a hint) rather than error when unprivileged.
		if !userMode && os.Geteuid() != 0 {
			log("plist-refresh: %s: needs a Homebrew ExecPath heal but rewriting a system LaunchDaemon requires root; re-run `sudo citadel update install`", pp)
			continue
		}

		if err := backupUnit(pp); err != nil {
			log("plist-refresh: %s: backup failed, skipping rewrite: %v", pp, err)
			continue
		}
		if err := writeUnitFile(pp, newContent); err != nil {
			log("plist-refresh: %s: write failed: %v", pp, err)
			continue
		}
		rewritten = append(rewritten, pp)
		log("plist-refresh: %s: repointed ExecPath to the stable Homebrew path %s (backup at %s.citadel-bak); takes effect on the next service restart", pp, stable, pp)
	}

	return rewritten, nil
}

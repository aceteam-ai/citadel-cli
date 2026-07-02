// Package composerefresh re-materializes citadel-owned embedded compose files
// on a node when the running binary version differs from the version that last
// materialized them (issue #426).
//
// Background: citadel materializes its embedded service compose templates
// (services.ServiceMap) into ~/citadel-node/services/<name>.yml the first time
// a node needs them, then treats them as create-once. A binary upgrade never
// rewrites the on-disk copies, so template changes -- the #405/#410 host-port
// fix (`${CITADEL_*_HOST_PORT:?...}`), image tags, healthchecks, GPU stanzas --
// never reach already-provisioned nodes. Those nodes keep stale, hardcoded-port
// composes forever and re-collide on the host ports #410 eliminated.
//
// This package runs a cheap version-gated sweep at boot that refreshes only the
// citadel-owned files, preserving operator hand-edits, and (optionally, and
// only when a service's published host port actually moved) force-recreates the
// affected container. See Sweep for the full contract.
package composerefresh

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// StampFile is the per-node record of what citadel last materialized. It lives
// alongside the compose files it describes (ServicesDir/StampFileName) and maps
// each citadel-owned service name to the sha256 of the embedded template content
// citadel last wrote for it. The Version field records the binary version that
// performed the last refresh so an unchanged boot is a cheap no-op.
const StampFileName = ".citadel-managed.json"

// managedStamp is the on-disk shape of StampFileName.
type managedStamp struct {
	// Version is the citadel binary version that last ran the refresh sweep.
	Version string `json:"version"`
	// Hashes maps service name -> sha256 hex of the embedded compose content
	// citadel last wrote to that file. Used to prove citadel (not an operator)
	// authored the current on-disk file before overwriting it.
	Hashes map[string]string `json:"hashes"`
}

// Logf is a minimal logging sink so the sweep can surface warnings (hand-edited
// files skipped, recreate hints) without importing the cmd logger.
type Logf func(format string, args ...any)

// PortRecreator is invoked after a managed compose file has been refreshed, for
// services whose host publish citadel owns. Implementations should compare the
// running container's published host port to wantHostPort and, if they differ
// AND recreation is enabled, force-recreate the service from composePath. It
// returns (recreated, err). A nil PortRecreator disables all recreation (pure
// file-refresh mode) -- used by tests and by the conservative default.
type PortRecreator func(service, composePath string, wantHostPort int) (bool, error)

// Options configures a Sweep.
type Options struct {
	// ServicesDir is the node's services directory (e.g.
	// ~/citadel-node/services). Compose files and the stamp file live here.
	ServicesDir string
	// Version is the running binary version. The sweep is a no-op when it
	// matches the stamped version.
	Version string
	// Embedded maps citadel-owned service name -> embedded compose template
	// content (services.ServiceMap). Only these files are ever touched.
	Embedded map[string]string
	// PortManaged maps citadel-owned service name -> the host port the current
	// template publishes (services.ServiceHostPorts). Services absent here are
	// still refreshed (templates change for image tags etc.) but never
	// force-recreated on a port change.
	PortManaged map[string]int
	// Recreator, when non-nil, may force-recreate a port-managed service whose
	// running host port moved. Nil => file-refresh only.
	Recreator PortRecreator
	// KnownHistoricalHashes maps service name -> set of sha256 hex hashes of
	// compose content that a PRIOR citadel binary is known to have shipped for
	// that service. It is the bootstrap safety net for pre-#426 nodes that have
	// no stamp: an on-disk file matching one of these hashes is provably a
	// citadel-written (unedited) old template, so it is safe to overwrite; a file
	// matching none is treated as operator-edited and preserved. When empty, the
	// bootstrap falls back to refreshing all citadel-owned files (issue-sanctioned
	// for these env-parameterized templates) with a loud warning.
	KnownHistoricalHashes map[string]map[string]bool
	// Log receives warnings/hints. May be nil.
	Log Logf
}

// Result summarizes what a Sweep did, for logging and tests.
type Result struct {
	// Skipped is true when the stamped version already matched Version, so the
	// sweep did no filesystem work.
	Skipped bool
	// Refreshed lists services whose on-disk compose file was rewritten from the
	// embedded template this run.
	Refreshed []string
	// Preserved lists citadel-owned services whose on-disk file was left
	// untouched because its content did not match the recorded citadel hash
	// (i.e. it was hand-edited).
	Preserved []string
	// Recreated lists services the Recreator force-recreated due to a host-port
	// change.
	Recreated []string
}

func (o Options) log(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

// Sweep refreshes citadel-owned compose files when the binary version changed.
//
// Contract:
//
//  1. No-op fast path: if the on-disk stamp records the same Version, return
//     immediately (Skipped=true). Boots on an unchanged binary do no work.
//
//  2. Only citadel-owned files (keys in Embedded) are ever read or written.
//     Operator-authored composes, sandbox overrides (*.sandbox.yml), .env files,
//     and anything outside Embedded are never touched.
//
//  3. Operator edits are preserved. A file is refreshed only when it is safe to
//     do so:
//     - the file is absent (materialize it), OR
//     - a stamp hash exists for it and the current on-disk content matches that
//     hash (citadel wrote it and nobody edited it), OR
//     - no stamp exists yet (bootstrap: pre-#426 nodes never wrote a stamp; the
//     embedded composes are citadel-owned and env-parameterized so refreshing
//     is safe -- we log a warning that hand-edits will be reset and then start
//     stamping so future upgrades use the precise hash-match rule).
//     If a stamp hash exists and the on-disk content does NOT match it, the file
//     was hand-edited: it is left untouched and a warning is logged.
//
//  4. Force-recreate is conservative and opt-in via a non-nil Recreator. Even
//     then, a service is only recreated when its file was refreshed AND its
//     running published host port differs from the new template's PortManaged
//     port. A file whose content is already current is never rewritten and never
//     disturbs a running container.
//
// The stamp is rewritten at the end with the new Version and the hashes of every
// embedded template (so the next boot can detect edits precisely). Sweep tries
// to be resilient: a failure on one service is logged and does not abort the
// others; only failing to persist the stamp is returned as an error.
func Sweep(opts Options) (Result, error) {
	var res Result

	stamp, stampExisted := readStamp(opts.ServicesDir)

	if stampExisted && stamp.Version == opts.Version {
		res.Skipped = true
		return res, nil
	}

	if err := os.MkdirAll(opts.ServicesDir, 0o755); err != nil {
		return res, fmt.Errorf("create services dir: %w", err)
	}

	if !stampExisted {
		opts.log("compose-refresh: no managed stamp found; treating citadel-owned composes as refreshable. Hand-edits to %s files will be reset.", "citadel-owned")
	}

	// Deterministic order for stable logs and tests.
	names := make([]string, 0, len(opts.Embedded))
	for name := range opts.Embedded {
		names = append(names, name)
	}
	sort.Strings(names)

	newHashes := make(map[string]string, len(names))

	for _, name := range names {
		content := opts.Embedded[name]
		newHash := hashContent(content)
		newHashes[name] = newHash

		destPath := filepath.Join(opts.ServicesDir, name+".yml")
		onDisk, readErr := os.ReadFile(destPath)

		switch {
		case os.IsNotExist(readErr):
			// Absent: materialize it. Nothing to recreate (no running container
			// from this file yet).
			if err := writeCompose(destPath, content); err != nil {
				opts.log("compose-refresh: %s: write failed: %v", name, err)
				continue
			}
			res.Refreshed = append(res.Refreshed, name)
			continue
		case readErr != nil:
			opts.log("compose-refresh: %s: read failed, skipping: %v", name, readErr)
			// Preserve whatever hash we had so we don't lose the record.
			if prev, ok := stamp.Hashes[name]; ok {
				newHashes[name] = prev
			}
			continue
		}

		onDiskHash := hashContent(string(onDisk))

		// Already current: never rewrite, never disturb the container.
		if onDiskHash == newHash {
			continue
		}

		recordedHash, haveRecord := stamp.Hashes[name]

		// Decide whether the on-disk file was written by citadel (safe to
		// overwrite) or hand-edited by the operator (must be preserved):
		//   - stamp present: citadel wrote it iff its content matches the recorded
		//     hash. A mismatch is a definitive hand-edit.
		//   - no stamp (pre-#426 node): use the known-historical-hash allowlist.
		//     A file byte-identical to a prior shipped template is citadel's; one
		//     matching nothing is treated as operator-edited and preserved. When
		//     no allowlist is supplied, fall back to refreshing (these templates
		//     are citadel-owned and env-parameterized) with a loud warning.
		var citadelWroteIt bool
		switch {
		case haveRecord:
			citadelWroteIt = onDiskHash == recordedHash
		case opts.KnownHistoricalHashes != nil:
			// Allowlist bootstrap: a known prior template is citadel's; anything
			// else is treated as an operator edit and preserved.
			citadelWroteIt = opts.KnownHistoricalHashes[name][onDiskHash]
		default:
			// No stamp and no allowlist: refresh (these templates are
			// citadel-owned and env-parameterized); the loud warning above already
			// told the operator hand-edits would be reset.
			citadelWroteIt = true
		}

		if !citadelWroteIt {
			// Hand-edited by the operator: leave it, warn, and record the CURRENT
			// on-disk hash so a future clean upgrade can re-adopt it if the
			// operator later restores an unedited template.
			res.Preserved = append(res.Preserved, name)
			opts.log("compose-refresh: %s: on-disk compose was hand-edited; leaving it untouched (delete it to accept the new citadel template)", name)
			newHashes[name] = onDiskHash
			continue
		}

		// Keep the prior on-disk content so we can roll back if a subsequent
		// force-recreate `up` fails on the new template -- a bad template must not
		// brick the service AND destroy the previously-working file.
		prevContent := onDisk
		if err := writeCompose(destPath, content); err != nil {
			opts.log("compose-refresh: %s: write failed: %v", name, err)
			continue
		}
		res.Refreshed = append(res.Refreshed, name)

		// Only port-managed services can trigger a recreate, and only when a
		// Recreator is wired in and the running host port actually moved.
		wantPort, portManaged := opts.PortManaged[name]
		if !portManaged || opts.Recreator == nil {
			if portManaged {
				// A plain citadel restart will NOT move a running container: the
				// start path no-ops on an already-running container. The operator
				// must recreate it (or unset CITADEL_COMPOSE_NO_RECREATE_ON_UPGRADE).
				opts.log("compose-refresh: %s: compose file refreshed to host port %d, but a running container is not moved automatically; run `citadel stop %s && citadel start %s` to apply (or unset CITADEL_COMPOSE_NO_RECREATE_ON_UPGRADE)", name, wantPort, name, name)
			}
			continue
		}
		recreated, err := opts.Recreator(name, destPath, wantPort)
		if err != nil {
			// The recreate `up` failed on the new template. Restore the prior file
			// and its recorded hash so the service is not left with a template that
			// can't come up, and so the next boot retries.
			opts.log("compose-refresh: %s: recreate failed, rolling back compose file: %v", name, err)
			if rbErr := writeCompose(destPath, string(prevContent)); rbErr != nil {
				opts.log("compose-refresh: %s: rollback write failed: %v", name, rbErr)
			}
			res.Refreshed = res.Refreshed[:len(res.Refreshed)-1]
			newHashes[name] = hashContent(string(prevContent))
			continue
		}
		if recreated {
			res.Recreated = append(res.Recreated, name)
		}
	}

	newStamp := managedStamp{Version: opts.Version, Hashes: newHashes}
	if err := writeStamp(opts.ServicesDir, newStamp); err != nil {
		return res, fmt.Errorf("persist managed stamp: %w", err)
	}

	return res, nil
}

func hashContent(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// writeCompose atomically writes compose content with 0600 perms (to protect
// any sensitive env vars, matching the existing materialize paths). It writes to
// a temp file in the same directory and renames into place so a crash mid-write
// can never leave a truncated compose file that would fail to parse.
func writeCompose(path, content string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".compose-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// readStamp loads StampFileName from dir. The second return is false when the
// file is absent or unreadable/corrupt (treated as "no prior stamp"), which the
// bootstrap rule in Sweep relies on.
func readStamp(dir string) (managedStamp, bool) {
	path := filepath.Join(dir, StampFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return managedStamp{Hashes: map[string]string{}}, false
	}
	var s managedStamp
	if err := json.Unmarshal(data, &s); err != nil {
		return managedStamp{Hashes: map[string]string{}}, false
	}
	if s.Hashes == nil {
		s.Hashes = map[string]string{}
	}
	return s, true
}

func writeStamp(dir string, s managedStamp) error {
	path := filepath.Join(dir, StampFileName)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

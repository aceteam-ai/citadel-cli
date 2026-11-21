// Package hpc owns the job-scoped HPC profile (aceteam#12715): where a batch
// allocation's citadel state lives, and the checks that keep two allocations
// of the same user on a shared home from ever sharing a path.
//
// The package is a leaf: it imports neither internal/platform nor
// internal/network. The cmd layer resolves a Layout here and then installs it
// as the process-wide path scope in those packages.
package hpc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Profile is the value of --profile / CITADEL_PROFILE that selects this mode.
const Profile = "hpc"

// Environment variable names read by ResolveLayout's callers. They are listed
// here so the cmd layer and tests agree on one spelling.
const (
	EnvJobID        = "SLURM_JOB_ID"
	EnvLegacyJobID  = "SLURM_JOBID"
	EnvRestartCount = "SLURM_RESTART_COUNT"
	EnvStateRoot    = "CITADEL_HPC_STATE_ROOT"
	EnvTmpDir       = "TMPDIR"
)

var (
	jobIDPattern   = regexp.MustCompile(`^[0-9]{1,20}$`)
	restartPattern = regexp.MustCompile(`^[0-9]{1,6}$`)
)

// Inputs are the already-read values that select a job root. They are passed
// in (not read from the environment here) so resolution is a pure function.
type Inputs struct {
	// JobID is SLURM_JOB_ID. Required.
	JobID string
	// LegacyJobID is SLURM_JOBID. When set it must equal JobID.
	LegacyJobID string
	// RestartCount is SLURM_RESTART_COUNT. A requeued job keeps its job id, so
	// a non-zero count selects a fresh root rather than reusing stale state.
	RestartCount string
	// StateRoot is the explicit scratch/job directory (flag or
	// CITADEL_HPC_STATE_ROOT). It wins over TmpDir.
	StateRoot string
	// TmpDir is TMPDIR, used only when StateRoot is empty.
	TmpDir string
}

// Layout is the resolved job-scoped directory tree. Every path is under Root.
type Layout struct {
	// Base is the existing directory the job root is created in.
	Base string
	// Root is <Base>/citadel-job-<id>[-r<restart>].
	Root string
	// ConfigDir replaces platform.ConfigDir() for this process.
	ConfigDir string
	// HomeDir replaces the user's home for this process (logs, defaults).
	HomeDir string
	// NodeDir replaces network.GetNodeConfigDir(): identity, worker lock,
	// usage store, device credentials. Network state is NodeDir/network.
	NodeDir string
	// MachineDir replaces the machine-wide config dir (/etc/citadel), so the
	// machine pointer and global config are never read from the host.
	MachineDir string
	// WorkspaceDir is the only directory job inputs and outputs may use.
	WorkspaceDir string
	// JobID is the validated Slurm job id.
	JobID string
}

// Dirs returns every directory in the layout below Root, in creation order.
func (l Layout) Dirs() []string {
	return []string{l.ConfigDir, l.HomeDir, l.NodeDir, l.MachineDir, l.WorkspaceDir}
}

// ResolveLayout validates the inputs and returns the job root layout. Any
// missing, malformed or ambiguous input is an error: the profile never falls
// back to the host's ordinary directories.
func ResolveLayout(in Inputs) (Layout, error) {
	jobID := strings.TrimSpace(in.JobID)
	legacy := strings.TrimSpace(in.LegacyJobID)
	if jobID == "" {
		if legacy != "" {
			return Layout{}, fmt.Errorf("hpc profile: %s is set but %s is not; refusing an ambiguous job id", EnvLegacyJobID, EnvJobID)
		}
		return Layout{}, fmt.Errorf("hpc profile: %s is not set; run inside a Slurm allocation", EnvJobID)
	}
	if !jobIDPattern.MatchString(jobID) {
		return Layout{}, fmt.Errorf("hpc profile: %s %q is not a numeric job id", EnvJobID, jobID)
	}
	if legacy != "" && legacy != jobID {
		return Layout{}, fmt.Errorf("hpc profile: %s and %s disagree; refusing an ambiguous job id", EnvJobID, EnvLegacyJobID)
	}

	name := "citadel-job-" + jobID
	if rc := strings.TrimSpace(in.RestartCount); rc != "" {
		if !restartPattern.MatchString(rc) {
			return Layout{}, fmt.Errorf("hpc profile: %s %q is not a number", EnvRestartCount, rc)
		}
		if strings.TrimLeft(rc, "0") != "" {
			name += "-r" + strings.TrimLeft(rc, "0")
		}
	}

	base := strings.TrimSpace(in.StateRoot)
	source := EnvStateRoot
	if base == "" {
		base = strings.TrimSpace(in.TmpDir)
		source = EnvTmpDir
	}
	if base == "" {
		return Layout{}, fmt.Errorf("hpc profile: no job state directory; set %s (or --hpc-state-root) or %s", EnvStateRoot, EnvTmpDir)
	}
	if !filepath.IsAbs(base) {
		return Layout{}, fmt.Errorf("hpc profile: %s %q must be an absolute path", source, base)
	}
	for _, part := range strings.Split(filepath.ToSlash(base), "/") {
		if part == ".." {
			return Layout{}, fmt.Errorf("hpc profile: %s %q must not contain '..'", source, base)
		}
	}
	base = filepath.Clean(base)

	root := filepath.Join(base, name)
	return Layout{
		Base:         base,
		Root:         root,
		ConfigDir:    filepath.Join(root, "config"),
		HomeDir:      filepath.Join(root, "home"),
		NodeDir:      filepath.Join(root, "node"),
		MachineDir:   filepath.Join(root, "machine"),
		WorkspaceDir: filepath.Join(root, "workspace"),
		JobID:        jobID,
	}, nil
}

// ErrUnsupportedPlatform is returned where owner-only checks cannot be made.
var ErrUnsupportedPlatform = errors.New("hpc profile is supported on Linux and macOS only")

// Prepare creates the layout with owner-only permissions and verifies every
// directory it reuses. uid is the caller's effective user id. The base must
// already exist; a world-writable base must carry the sticky bit (as /tmp
// does) so another user cannot replace the job root.
func Prepare(l Layout, uid int) error {
	if uid == 0 {
		return errors.New("hpc profile: refusing to run as root")
	}
	if l.Root == "" || l.Base == "" {
		return errors.New("hpc profile: empty layout")
	}
	info, err := os.Stat(l.Base)
	if err != nil {
		return fmt.Errorf("hpc profile: job state base %s: %w", l.Base, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("hpc profile: job state base %s is not a directory", l.Base)
	}
	if err := checkReplaceSafe(l.Base, info); err != nil {
		return err
	}
	if err := checkAncestorsReplaceSafe(l.Base); err != nil {
		return err
	}
	if err := ensurePrivateDir(l.Root, uid); err != nil {
		return err
	}
	for _, dir := range l.Dirs() {
		if err := ensurePrivateDir(dir, uid); err != nil {
			return err
		}
	}
	return nil
}

// checkReplaceSafe refuses a directory in which another user could rename or
// replace the job root: any group or other write bit without the sticky bit.
// Project scratch is often 2770 or 0775; a group member there could swap the
// job root for their own directory or a symlink.
func checkReplaceSafe(dir string, info os.FileInfo) error {
	if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("hpc profile: %s is writable by group or other without the sticky bit (%#o); another user could replace the job root. Use a private directory or one with the sticky bit", dir, info.Mode().Perm())
	}
	return nil
}

// checkAncestorsReplaceSafe applies checkReplaceSafe to every ancestor of
// base after symlinks are resolved, so a writable parent cannot rename the
// base itself away. The filesystem root is checked like any other directory.
func checkAncestorsReplaceSafe(base string) error {
	resolved, err := filepath.EvalSymlinks(base)
	if err != nil {
		return fmt.Errorf("hpc profile: resolve %s: %w", base, err)
	}
	for dir := filepath.Dir(resolved); ; dir = filepath.Dir(dir) {
		info, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("hpc profile: stat %s: %w", dir, err)
		}
		if err := checkReplaceSafe(dir, info); err != nil {
			return err
		}
		if dir == filepath.Dir(dir) {
			return nil
		}
	}
}

// ensurePrivateDir creates dir with mode 0700 or verifies an existing one is
// a real directory (not a symlink), owned by uid, with no group/other bits.
func ensurePrivateDir(dir string, uid int) error {
	if err := os.Mkdir(dir, 0o700); err == nil {
		// Mkdir is subject to umask, which can only clear bits; assert anyway.
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("hpc profile: chmod %s: %w", dir, err)
		}
	} else if !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("hpc profile: create %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("hpc profile: stat %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("hpc profile: %s exists and is not a plain directory", dir)
	}
	owner, err := ownerUID(info)
	if err != nil {
		return err
	}
	if owner != uid {
		return fmt.Errorf("hpc profile: %s is owned by uid %d, not %d", dir, owner, uid)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("hpc profile: %s has group or other permissions (%#o); expected 0700", dir, info.Mode().Perm())
	}
	return nil
}

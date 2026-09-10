// internal/agentsprobe/resolver.go
//
// The layered "owner signal" resolver for the S2 worker (aceteam #8993). Inside
// a long-lived `citadel work` process neither HOME nor PATH is the target's, so
// this decides WHICH user's environment Probe inspects, first match wins:
//
//  1. agents_probe_user (explicit config override; a lookup failure is an ERROR)
//  2. owner uid of the node config dir (the fleet common case; home from passwd,
//     NOT the dir path)
//  3. SUDO_USER (the existing #1012 branch; "root" treated as unset)
//  4. process user (the #1012 fallback)
//
// Each Target carries an attributable Signal so a consumer can tell "probed
// jason's environment" from "probed root's environment on a root-owned node",
// the two outcomes that look identical today. This package stays stdlib-only
// (leaf constraint): the node config dir is passed IN as a string, never resolved
// via internal/network here.
package agentsprobe

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
)

// TargetSignal names which resolver tier produced a Target. It is not
// decorative: it is what makes a root-owned-node probe attributable rather than
// ambiguous.
type TargetSignal string

const (
	SignalConfig       TargetSignal = "config"
	SignalNodeDirOwner TargetSignal = "node-dir-owner"
	SignalSudoUser     TargetSignal = "sudo-user"
	SignalProcess      TargetSignal = "process"
)

// Target is the account whose HOME/PATH the probe inspects, plus the evidence
// (Signal) and the drop credentials (UID/GID) the worker uses.
type Target struct {
	Username string
	UID      int
	GID      int
	HomeDir  string
	PathEnv  string
	Signal   TargetSignal
}

// ResolveInputs are the (already-read) inputs to the resolver, so the logic is a
// pure function testable without touching a real HOME, passwd, or filesystem.
type ResolveInputs struct {
	// ConfiguredUser is the manifest agents_probe_user override, or "".
	ConfiguredUser string
	// StateDir is network.GetStateDir() (the <nodeConfigDir>/network dir, chowned
	// to the owner in every init branch that chowns), stat'd first.
	StateDir string
	// NodeConfigDir is network.GetNodeConfigDir(), the fallback owner-dir to stat.
	NodeConfigDir string
	// SudoUser is os.Getenv("SUDO_USER").
	SudoUser string
	// ProcessUID is os.Getuid().
	ProcessUID int
}

// resolveDeps injects the passwd/stat/home lookups so every branch is
// exercisable without privileges or real users (the resolveTargetUser pattern).
type resolveDeps struct {
	lookupName  func(string) (*user.User, error)
	lookupUID   func(int) (*user.User, error)
	statOwner   func(string) (int, bool)
	processHome func() (string, error)
	processPath string
}

// ResolveTargetUserForNode wires the real lookups and runs the layered resolver.
// An error means no target could be resolved (e.g. a configured user missing
// from passwd); the caller probes with HomeUnknown and records the error rather
// than probing the worker's own /root.
func ResolveTargetUserForNode(in ResolveInputs) (Target, error) {
	return resolveTargetUserForNode(in, resolveDeps{
		lookupName:  user.Lookup,
		lookupUID:   func(uid int) (*user.User, error) { return user.LookupId(strconv.Itoa(uid)) },
		statOwner:   statOwnerUID,
		processHome: os.UserHomeDir,
		processPath: os.Getenv("PATH"),
	})
}

func resolveTargetUserForNode(in ResolveInputs, d resolveDeps) (Target, error) {
	// (1) Explicit override wins. A configured name that does not resolve is an
	// ERROR, never a silent fallthrough to some other user.
	if name := strings.TrimSpace(in.ConfiguredUser); name != "" {
		u, err := d.lookupName(name)
		if err != nil {
			return Target{}, fmt.Errorf("agents_probe_user %q: %w", name, err)
		}
		return targetFromUser(u, SignalConfig, d.processPath)
	}

	// (2) Owner uid of the node config dir. HOME comes from passwd, NOT the dir
	// path (the dir can live outside the owner's home in the /etc/citadel shapes).
	// Any failure (unstat-able dir, uid with no passwd entry, unparsable uid)
	// falls through -- never an error, since a lower tier may still resolve.
	for _, dir := range []string{in.StateDir, in.NodeConfigDir} {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		uid, ok := d.statOwner(dir)
		if !ok {
			continue
		}
		u, err := d.lookupUID(uid)
		if err != nil {
			continue
		}
		if t, err := targetFromUser(u, SignalNodeDirOwner, d.processPath); err == nil {
			return t, nil
		}
	}

	// (3) SUDO_USER (the existing #1012 branch), treating "root" as unset.
	if su := strings.TrimSpace(in.SudoUser); su != "" && su != "root" {
		u, err := d.lookupName(su)
		if err != nil {
			return Target{}, fmt.Errorf("resolve home for SUDO_USER %q: %w", su, err)
		}
		return targetFromUser(u, SignalSudoUser, d.processPath)
	}

	// (4) Process user. HomeDir from the process's own $HOME (exactly #1012's
	// fallback). Username/GID are best-effort from passwd so the drop decision has
	// a name -- but the process tier never drops (target == process), so a lookup
	// failure here is harmless.
	home, err := d.processHome()
	if err != nil {
		return Target{}, fmt.Errorf("resolve process home directory: %w", err)
	}
	t := Target{
		UID:     in.ProcessUID,
		HomeDir: home,
		PathEnv: userPathEnv(home, d.processPath),
		Signal:  SignalProcess,
	}
	if u, err := d.lookupUID(in.ProcessUID); err == nil {
		t.Username = u.Username
		if gid, err := strconv.Atoi(u.Gid); err == nil {
			t.GID = gid
		}
	}
	return t, nil
}

// targetFromUser builds a Target from a passwd entry. A non-integer uid (a
// Windows SID) is an error -- there is no numeric credential to drop to there,
// which is fine because the drop is POSIX-only and the Windows worker resolves
// via the process tier anyway.
func targetFromUser(u *user.User, signal TargetSignal, processPath string) (Target, error) {
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return Target{}, fmt.Errorf("parse uid %q for %q: %w", u.Uid, u.Username, err)
	}
	gid, _ := strconv.Atoi(u.Gid) // non-fatal: the drop still sets the uid
	return Target{
		Username: u.Username,
		UID:      uid,
		GID:      gid,
		HomeDir:  u.HomeDir,
		PathEnv:  userPathEnv(u.HomeDir, processPath),
		Signal:   signal,
	}, nil
}

// probeOptions builds the Probe Options for this resolved target, deciding
// whether to drop privileges. The drop happens ONLY when the worker is root
// (processUID == 0) and the target is a different, non-root account -- exactly
// the fleet root-worker case where the target's PATH starts with directories the
// target user WRITES (~/.npm-global/bin), so exec'ing them as root would be a
// local privilege escalation (S2 design 2d). A non-root worker has no privilege
// to drop, so it runs normally.
func (t Target) probeOptions(processUID int) Options {
	opts := Options{HomeDir: t.HomeDir, PathEnv: t.PathEnv}
	if processUID == 0 && t.UID != 0 && t.UID != processUID {
		opts.DropTo = &Credential{UID: t.UID, GID: t.GID, Username: t.Username}
	}
	return opts
}

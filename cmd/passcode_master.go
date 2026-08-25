// cmd/passcode_master.go
//
// Master PIN (aceteam-ai/citadel-cli#796): the node master PIN both gates
// online access and roots zero-knowledge at-rest encryption. It is set LOCALLY
// only (this CLI / the TUI / a local prompt) — never via a platform push. These
// subcommands are the local set/rotate/enroll surface; the crypto lives in
// internal/nodevault and the legacy-gate reconciliation in internal/config.
package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var ackDataLossFlag bool
var assumeNoLegacyPasscodeFlag bool

var passcodeEnrollCmd = &cobra.Command{
	Use:   "enroll",
	Short: "Migrate this node to the master PIN (one-time, deletes the legacy passcode)",
	Long: `Enrolls the node master PIN (aceteam-ai/citadel-cli#796).

The master PIN both gates online access (Console/Desktop/Files/Shell) AND is the
only key to any data this node encrypts at rest. Enrollment is a one-time
migration off the legacy bcrypt passcode: you re-enter the current passcode (to
prove ownership) and choose a new master PIN, after which the legacy passcode
hash is deleted — a bcrypt hash cannot yield an encryption key, and leaving it
behind would remain a cheap offline brute-force target.

WARNING: if you forget the master PIN, or the node's on-disk pepper file is
lost, any data encrypted under it is permanently unrecoverable — not by you, not
by AceTeam. This is the zero-knowledge property working as designed.

The master PIN also grants full remote access to this machine (console, screen,
files, shell). Anyone who holds it has the same power. Set it locally only.`,
	Args: cobra.NoArgs,
	RunE: runPasscodeEnroll,
}

var passcodeRotateCmd = &cobra.Command{
	Use:   "rotate",
	Short: "Change the master PIN (re-wraps the encryption key; no data re-encryption)",
	Long: `Rotates the node master PIN. Re-derives the key from the new PIN and re-wraps
the existing data-encryption key — already-encrypted data is untouched and is
NOT re-encrypted. Requires the current master PIN.`,
	Args: cobra.NoArgs,
	RunE: runPasscodeRotate,
}

var passcodeStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show master-PIN and at-rest-encryption status",
	Args:  cobra.NoArgs,
	RunE:  runPasscodeStatus,
}

func init() {
	passcodeEnrollCmd.Flags().BoolVar(&ackDataLossFlag, "ack-data-loss", false,
		"acknowledge that forgetting the master PIN (or losing the node pepper) permanently destroys encrypted data (required for non-interactive/piped use)")
	passcodeEnrollCmd.Flags().BoolVar(&assumeNoLegacyPasscodeFlag, "assume-no-legacy-passcode", false,
		"override a failed check for an existing legacy node passcode (e.g. a candidate config location could not be read) and enroll as if none was ever set; only pass this if you are certain")
	passcodeRotateCmd.Flags().BoolVar(&ackDataLossFlag, "ack-data-loss", false,
		"acknowledge the data-loss risk (required for non-interactive/piped use)")
	passcodeCmd.AddCommand(passcodeEnrollCmd)
	passcodeCmd.AddCommand(passcodeRotateCmd)
	passcodeCmd.AddCommand(passcodeStatusCmd)
}

// errAlreadyEnrolled is returned when enroll is invoked on a node that already
// has a master PIN and nothing left to migrate.
var errAlreadyEnrolled = errors.New("a master PIN is already enrolled; use 'citadel passcode rotate' to change it")

// errLegacyCheckInconclusive is returned when the legacy-passcode ownership
// check could not read every plausible ConfigDir on this machine (citadel#808
// fast-follow). platform.ConfigDir() is invoker-scoped, but the master-PIN
// vault enroll installs is machine-convergent (network.GetNodeConfigDir()):
// if enroll trusted an incomplete "no legacy hash found" answer, it would
// install a brand-new master PIN WITHOUT proving the existing passcode, and
// that PIN would immediately become the binding gate answer for every
// process on the machine — an authorization bypass. So an ambiguous scan
// fails closed instead of being treated as "no passcode was ever set".
var errLegacyCheckInconclusive = errors.New(
	"could not conclusively determine whether a legacy node passcode already exists on this machine " +
		"(a candidate config location could not be read); refusing to enroll without proof. " +
		"Re-run with privileges that can read it, or pass --assume-no-legacy-passcode if you are certain " +
		"no legacy passcode was ever set on this node")

// enrollPrecheck decides how to handle 'passcode enroll' given current state.
// finishCleanup=true means a prior migration was interrupted (the vault is set
// but the legacy bcrypt hash still lingers) and enroll should just delete that
// leftover hash rather than refusing — otherwise the lingering hash, the exact
// brute-force target enrollment deletes, could never be cleaned up.
func enrollPrecheck(vaultConfigured, hasLegacyHash bool) (finishCleanup bool, err error) {
	if !vaultConfigured {
		return false, nil // normal enrollment path
	}
	if hasLegacyHash {
		return true, nil // finish an interrupted migration
	}
	return false, errAlreadyEnrolled
}

// legacyOwnershipGate decides whether enroll may trust the legacy-passcode
// scan at all, split out as a pure function (mirrors enrollPrecheck) so the
// ambiguous-scan fail-closed behavior is unit-testable without touching the
// filesystem.
//
// Inconclusive blocks REGARDLESS of Found. It is tempting to only refuse when
// nothing was found (a found hash "already" requires legacyVerify to pass) —
// but that reopens the exact bypass this check exists to close: an invoker
// who can write to even ONE readable candidate directory (e.g. their own
// ~/.citadel-cli) can plant a hash of a PIN they know, which makes
// Found()==true, while the REAL hash sits in a candidate they cannot read
// (Inconclusive==true). If Inconclusive were only checked when nothing was
// found, that planted hash would sail through as "the" legacy proof. So an
// incomplete scan is never trustworthy, whether or not it also turned up a
// hash — only a scan that read every candidate cleanly may be trusted, and
// only the operator's explicit override may stand in for that.
func legacyOwnershipGate(legacy config.LegacyPasscodeSearchResult, assumeNoLegacy bool) error {
	if legacy.Inconclusive && !assumeNoLegacy {
		return errLegacyCheckInconclusive
	}
	return nil
}

// legacyClearDirs returns the set of directories clearLegacyPasscodeAt should
// clear: every directory FindLegacyPasscode actually found a hash in, plus
// the current invocation's own platform.ConfigDir() (preserving the
// pre-#808-fix behavior of always touching it, so permissions.yaml keeps
// materializing there even on a fresh node with nothing to clear).
func legacyClearDirs(legacy config.LegacyPasscodeSearchResult) []string {
	dirs := append([]string{platform.ConfigDir()}, legacy.Dirs...)
	seen := make(map[string]bool, len(dirs))
	out := dirs[:0]
	for _, d := range dirs {
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// clearLegacyPasscodeAt deletes the legacy bcrypt PasscodeHash from every
// directory in dirs. Idempotent per-directory (SetPasscode("") on an already
// empty hash is a no-op), so it is safe to call on a superset of dirs that
// actually have a hash, and safe for Enroll to re-run to finish an
// interrupted migration. It keeps going past a single directory's failure —
// one unwritable candidate (e.g. a system path this invocation can't write)
// must not block clearing the others — and joins every failure into the
// returned error so none is silently swallowed.
func clearLegacyPasscodeAt(dirs []string) error {
	var errs []error
	for _, dir := range dirs {
		p := config.LoadPermissions(dir)
		_ = p.SetPasscode("") // empty clears the hash; never re-hashes (guard-safe)
		if err := config.SavePermissions(dir, p); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", dir, err))
		}
	}
	return errors.Join(errs...)
}

// legacyWrongPINMessage explains an Enroll ErrWrongPIN in terms the operator
// can act on. Verify's AND semantics (see LegacyPasscodeSearchResult.Verify)
// mean this can trigger for a reason OTHER than a typo: if legacy hashes were
// found in more than one directory (nothing in this codebase keeps
// permissions.yaml in sync across invocation contexts — see "ConfigDir() is
// invoker-scoped" in CLAUDE.md), and they are for DIFFERENT PINs, no single
// PIN can ever satisfy all of them, and the plain "incorrect" message would
// be a dead end. Naming the directories turns that into a fixable state: the
// operator can inspect/remove the stale permissions.yaml files by hand.
func legacyWrongPINMessage(legacy config.LegacyPasscodeSearchResult) string {
	if len(legacy.Dirs) <= 1 {
		return "current node passcode is incorrect"
	}
	return fmt.Sprintf(
		"current node passcode is incorrect, OR a different legacy passcode hash was found in each of: %s "+
			"(nothing keeps permissions.yaml in sync across invocation contexts — see CLAUDE.md 'ConfigDir() is "+
			"invoker-scoped'). If these are stale copies from an old context, remove the wrong permissions.yaml "+
			"by hand and re-run enroll.",
		strings.Join(legacy.Dirs, ", "))
}

func runPasscodeEnroll(cmd *cobra.Command, args []string) error {
	v := masterVault()

	// The legacy passcode is invoker-scoped (platform.ConfigDir() resolves
	// differently per invocation context — see CLAUDE.md "ConfigDir() is
	// invoker-scoped" / citadel#696), but the master-PIN vault this command
	// installs is machine-convergent (network.GetNodeConfigDir()). Scanning
	// only the current invocation's ConfigDir() could miss a legacy passcode
	// set under a different context (e.g. a systemd-root 'citadel work'
	// applying APPLY_DEVICE_CONFIG), letting enroll skip the ownership proof
	// entirely (citadel#808 fast-follow). So check every plausible ConfigDir.
	candidates := platform.ConfigDirCandidates()
	legacy := config.FindLegacyPasscode(candidates)
	hasLegacyHash := legacy.Found()

	if finishCleanup, err := enrollPrecheck(v.IsConfigured(), hasLegacyHash); err != nil {
		return err
	} else if finishCleanup {
		if err := clearLegacyPasscodeAt(legacyClearDirs(legacy)); err != nil {
			return fmt.Errorf("finish interrupted migration (delete legacy passcode): %w", err)
		}
		fmt.Println("Master PIN already enrolled; cleaned up the leftover legacy passcode from an interrupted migration.")
		return nil
	}

	// About to enroll fresh: if the scan could not read every candidate, the
	// result — found or not — is not trustworthy proof of what legacy
	// passcodes actually exist on this machine. Refuse rather than risk
	// installing an unauthenticated master PIN or accepting a hash an
	// unprivileged invoker could have planted themselves.
	if err := legacyOwnershipGate(legacy, assumeNoLegacyPasscodeFlag); err != nil {
		return err
	}

	isTTY := term.IsTerminal(int(os.Stdin.Fd()))
	printMasterPINDisclosure()
	if err := confirmDataLossAck(isTTY); err != nil {
		return err
	}

	policy := nodevault.LoadPolicy(network.GetNodeConfigDir())

	// Prove ownership against the legacy passcode when one is set.
	var legacyVerify func(string) bool
	var legacyPIN string
	if hasLegacyHash {
		var err error
		legacyPIN, err = readSecretOnce("Current node passcode: ", isTTY)
		if err != nil {
			return err
		}
		legacyVerify = legacy.Verify
	}

	newPIN, err := readSecretConfirmed("New master PIN: ", "Confirm master PIN: ", isTTY)
	if err != nil {
		return err
	}

	deleteLegacy := func() error { return clearLegacyPasscodeAt(legacyClearDirs(legacy)) }
	if err := v.Enroll(legacyPIN, newPIN, policy, true, legacyVerify, deleteLegacy); err != nil {
		if errors.Is(err, nodevault.ErrWrongPIN) {
			return errors.New(legacyWrongPINMessage(legacy))
		}
		return fmt.Errorf("enroll master PIN: %w", err)
	}

	fmt.Println("Master PIN enrolled. The legacy node passcode has been deleted.")
	printBadgeLine(v.Status())
	fmt.Println("Takes effect immediately: gates verify against the master PIN on their next request.")
	return nil
}

func runPasscodeRotate(cmd *cobra.Command, args []string) error {
	v := masterVault()
	if !v.IsConfigured() {
		return fmt.Errorf("no master PIN is enrolled; run 'citadel passcode enroll' first")
	}

	isTTY := term.IsTerminal(int(os.Stdin.Fd()))
	if err := confirmDataLossAck(isTTY); err != nil {
		return err
	}

	oldPIN, err := readSecretOnce("Current master PIN: ", isTTY)
	if err != nil {
		return err
	}
	newPIN, err := readSecretConfirmed("New master PIN: ", "Confirm master PIN: ", isTTY)
	if err != nil {
		return err
	}

	policy := nodevault.LoadPolicy(network.GetNodeConfigDir())
	if err := v.ChangePIN(oldPIN, newPIN, policy, true); err != nil {
		if errors.Is(err, nodevault.ErrWrongPIN) {
			return fmt.Errorf("current master PIN is incorrect")
		}
		if nodevault.IsLockedOut(err) {
			return err
		}
		return fmt.Errorf("rotate master PIN: %w", err)
	}
	fmt.Println("Master PIN rotated. Existing encrypted data was re-keyed without re-encryption.")
	printBadgeLine(v.Status())
	return nil
}

func runPasscodeStatus(cmd *cobra.Command, args []string) error {
	v := masterVault()
	st := v.Status()
	if !st.Configured {
		perms := config.LoadPermissions(platform.ConfigDir())
		if perms.PasscodeHash != "" {
			fmt.Println("Master PIN: not enrolled (legacy node passcode is set; run 'citadel passcode enroll' to migrate).")
		} else {
			fmt.Println("Master PIN: not set.")
		}
		return nil
	}
	fmt.Println("Master PIN: enrolled.")
	printBadgeLine(st)
	lo := v.LockoutStatus()
	if lo.LockedOut {
		fmt.Printf("Lockout: ACTIVE until %s (%d failed attempts).\n", lo.LockoutUntil.Format("15:04:05"), lo.FailedAttempts)
	} else if lo.FailedAttempts > 0 {
		fmt.Printf("Failed attempts since last success: %d.\n", lo.FailedAttempts)
	}
	return nil
}

// printBadgeLine reports the entropy-gated end-to-end-encryption badge honestly.
func printBadgeLine(st nodevault.Status) {
	if st.MeetsThreshold {
		fmt.Printf("At-rest encryption: end-to-end encrypted (secret entropy ~%.0f bits meets the threshold).\n", st.EntropyBits)
	} else {
		fmt.Printf("At-rest encryption: encrypted, but the secret is short (~%.0f bits) — brute-forceable if the disk is stolen. Set a passphrase for an unqualified end-to-end-encrypted guarantee.\n", st.EntropyBits)
	}
}

func printMasterPINDisclosure() {
	fmt.Fprintln(os.Stderr, strings.TrimSpace(`
================================ READ CAREFULLY ================================
The node master PIN grants FULL access to this machine over the network:
console/terminal, desktop/screen, files, and shell. Anyone who holds it can do
anything you can do on this box.

It is ALSO the only key to any data this node encrypts at rest. If you forget it
- or lose the node's on-disk pepper file - that data is PERMANENTLY
unrecoverable. Not by you. Not by AceTeam. There is no recovery path by design.
===============================================================================`))
}

// confirmDataLossAck enforces the typed acknowledgement of data-loss risk. On a
// terminal, the user must type the exact phrase; non-interactively, the
// --ack-data-loss flag stands in (so a scripted enrollment still records intent).
func confirmDataLossAck(isTTY bool) error {
	if ackDataLossFlag {
		return nil
	}
	if !isTTY {
		return fmt.Errorf("refusing to set a master PIN non-interactively without --ack-data-loss (forgetting it destroys encrypted data)")
	}
	const phrase = "I UNDERSTAND"
	fmt.Fprintf(os.Stderr, "Type '%s' to confirm you accept permanent data loss if the PIN is forgotten: ", phrase)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read acknowledgement: %w", err)
	}
	if strings.TrimSpace(line) != phrase {
		return fmt.Errorf("acknowledgement not given; aborting")
	}
	return nil
}

// readSecretOnce reads a secret with no confirmation (used for proving an
// existing PIN). No-echo on a terminal; a single trimmed line when piped.
func readSecretOnce(prompt string, isTTY bool) (string, error) {
	if !isTTY {
		return readPasscodeFromReader(os.Stdin)
	}
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read secret: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// readSecretConfirmed reads a new secret twice (no-echo) and requires a match.
// When piped, it reads one line with no re-prompt.
func readSecretConfirmed(prompt, confirmPrompt string, isTTY bool) (string, error) {
	if !isTTY {
		return readPasscodeFromReader(os.Stdin)
	}
	fd := int(os.Stdin.Fd())
	fmt.Fprint(os.Stderr, prompt)
	first, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read secret: %w", err)
	}
	fmt.Fprint(os.Stderr, confirmPrompt)
	second, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read secret confirmation: %w", err)
	}
	a, b := strings.TrimSpace(string(first)), strings.TrimSpace(string(second))
	if a != b {
		return "", fmt.Errorf("entries did not match")
	}
	return a, nil
}

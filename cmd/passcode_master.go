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
	passcodeRotateCmd.Flags().BoolVar(&ackDataLossFlag, "ack-data-loss", false,
		"acknowledge the data-loss risk (required for non-interactive/piped use)")
	passcodeCmd.AddCommand(passcodeEnrollCmd)
	passcodeCmd.AddCommand(passcodeRotateCmd)
	passcodeCmd.AddCommand(passcodeStatusCmd)
}

func runPasscodeEnroll(cmd *cobra.Command, args []string) error {
	v := masterVault()
	if v.IsConfigured() {
		return fmt.Errorf("a master PIN is already enrolled; use 'citadel passcode rotate' to change it")
	}

	isTTY := term.IsTerminal(int(os.Stdin.Fd()))
	printMasterPINDisclosure()
	if err := confirmDataLossAck(isTTY); err != nil {
		return err
	}

	policy := nodevault.LoadPolicy(network.GetNodeConfigDir())

	// Prove ownership against the legacy passcode when one is set. The vault is
	// not configured yet, so VerifyPasscode still runs the legacy bcrypt path.
	perms := config.LoadPermissions(platform.ConfigDir())
	var legacyVerify func(string) bool
	var legacyPIN string
	if perms.HasPasscode() {
		var err error
		legacyPIN, err = readSecretOnce("Current node passcode: ", isTTY)
		if err != nil {
			return err
		}
		legacyVerify = perms.VerifyPasscode
	}

	newPIN, err := readSecretConfirmed("New master PIN: ", "Confirm master PIN: ", isTTY)
	if err != nil {
		return err
	}

	deleteLegacy := func() error {
		p := config.LoadPermissions(platform.ConfigDir())
		_ = p.SetPasscode("") // empty clears the legacy hash; never re-hashes
		return config.SavePermissions(platform.ConfigDir(), p)
	}

	if err := v.Enroll(legacyPIN, newPIN, policy, true, legacyVerify, deleteLegacy); err != nil {
		if errors.Is(err, nodevault.ErrWrongPIN) {
			return fmt.Errorf("current node passcode is incorrect")
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

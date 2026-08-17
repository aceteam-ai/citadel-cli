// cmd/passcode.go
//
// citadel#753: before this command existed, the only ways to set the per-node
// passcode that gates Console/Desktop/Files/Shell (aceteam#6524) were an
// APPLY_DEVICE_CONFIG push from the platform or the Control Center TUI. A node
// that enabled one of those surfaces any other way (e.g. by hand-editing
// permissions.yaml, or on a headless box with no Control Center session) had
// no CLI path to set the passcode those surfaces require, so they stayed
// unreachable while doctor/heartbeat kept reporting healthy.
package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var passcodeCmd = &cobra.Command{
	Use:   "passcode",
	Short: "Manage the node passcode that gates Console/Desktop/Files/Shell access",
	Long: `The node passcode (aceteam#6524) gates the sensitive remote-access surfaces
(Console/terminal, Desktop/VNC/screen, Files, and Shell/SHELL_COMMAND) once an
operator has enabled them. Enabling a surface is not enough by itself: without
a passcode set, that surface fails closed (access denied) even though it is
enabled.

Use 'citadel passcode set' to set or change it, and 'citadel passcode clear'
to remove it (which re-locks every enabled sensitive surface).`,
}

var passcodeSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Set or change the node passcode",
	Long: `Sets the per-node passcode that gates Console/Desktop/Files/Shell remote
access (aceteam#6524). This is what a client (e.g. 'citadel connect') must present,
in addition to a valid token or verified mesh-peer identity, before an
enabled sensitive surface will actually respond.

Takes effect immediately, without restarting the worker: every gate
(terminal server, gateway, SHELL_COMMAND handler) reloads permissions.yaml on
each connection, so a running 'citadel work' picks up the new passcode on its
very next request.

The PIN is read from a no-echo interactive prompt (with a confirmation
re-entry), or piped in on stdin when stdin is not a terminal. It is never
accepted as a command-line argument, so it can't land in shell history or be
visible to other users on the machine via 'ps'.`,
	Example: `  # Interactive (prompts twice, no echo)
  citadel passcode set

  # Piped (e.g. from a secrets manager); no confirmation re-prompt
  echo -n "1234" | citadel passcode set`,
	Args: cobra.NoArgs,
	RunE: runPasscodeSet,
}

var passcodeClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "Clear the node passcode",
	Long: `Clears the per-node passcode. This FAILS CLOSED: with no passcode set, every
sensitive remote-access surface (Console/Desktop/Files/Shell) stays denied even
if it is enabled, until 'citadel passcode set' is run again.

Takes effect immediately, without restarting the worker.`,
	Args: cobra.NoArgs,
	RunE: runPasscodeClear,
}

func init() {
	rootCmd.AddCommand(passcodeCmd)
	passcodeCmd.AddCommand(passcodeSetCmd)
	passcodeCmd.AddCommand(passcodeClearCmd)
}

func runPasscodeSet(cmd *cobra.Command, args []string) error {
	pin, err := readPasscodeInput(os.Stdin, term.IsTerminal(int(os.Stdin.Fd())))
	if err != nil {
		return err
	}

	perms, err := setNodePasscode(platform.ConfigDir(), pin)
	if err != nil {
		return err
	}

	fmt.Println("Node passcode set.")
	fmt.Println("Takes effect immediately, no worker restart needed (the gate reloads permissions.yaml per connection).")
	if !(perms.Console || perms.Desktop || perms.Files || perms.Shell) {
		fmt.Println("Note: Console, Desktop, Files, and Shell are all currently disabled, so this passcode has nothing to gate yet.")
	}
	return nil
}

func runPasscodeClear(cmd *cobra.Command, args []string) error {
	perms, err := clearNodePasscode(platform.ConfigDir())
	if err != nil {
		return err
	}

	fmt.Println("Node passcode cleared.")
	// Shell is included here (citadel#763): internal/jobs/shell_command.go gates
	// an enabled Shell handler on VerifyPasscode the same way Console/Desktop/
	// Files are gated, so this warning must name it too.
	if perms.Console || perms.Desktop || perms.Files || perms.Shell {
		fmt.Println("Console, Desktop, Files, and/or Shell are enabled; they now fail closed (access denied) until a new passcode is set.")
	}
	fmt.Println("Takes effect immediately, no worker restart needed.")
	return nil
}

// setNodePasscode loads permissions from configDir, hashes and stores pin,
// and persists the result. It refuses an empty/whitespace-only pin ('citadel
// passcode clear' is the explicit way to remove a passcode, so 'set' with
// nothing typed is far more likely a mistake than an intent to lock the
// node). Returns the updated permissions so callers can report follow-on
// state (e.g. whether any sensitive surface is actually enabled) without a
// second load.
func setNodePasscode(configDir, pin string) (*config.Permissions, error) {
	if strings.TrimSpace(pin) == "" {
		return nil, fmt.Errorf("passcode must not be empty (use 'citadel passcode clear' to remove it)")
	}

	perms := config.LoadPermissions(configDir)
	if err := perms.SetPasscode(pin); err != nil {
		return nil, fmt.Errorf("set passcode: %w", err)
	}
	if err := config.SavePermissions(configDir, perms); err != nil {
		return nil, fmt.Errorf("save permissions: %w", err)
	}
	return perms, nil
}

// clearNodePasscode loads permissions from configDir, clears the passcode
// hash, and persists the result.
func clearNodePasscode(configDir string) (*config.Permissions, error) {
	perms := config.LoadPermissions(configDir)
	_ = perms.SetPasscode("") // empty pin only clears; SetPasscode never errors on this path
	if err := config.SavePermissions(configDir, perms); err != nil {
		return nil, fmt.Errorf("save permissions: %w", err)
	}
	return perms, nil
}

// readPasscodeInput reads the PIN for 'citadel passcode set'. When stdin is
// not a terminal (piped/redirected), it reads a single trimmed line with no
// confirmation re-prompt (there is nothing to re-prompt on a non-interactive
// stream). When stdin IS a terminal, it prompts twice with no echo
// (golang.org/x/term.ReadPassword) and requires the two entries to match, so
// an interactive typo does not silently lock the node behind a passcode the
// operator never actually typed.
func readPasscodeInput(stdin *os.File, isTerminal bool) (string, error) {
	if !isTerminal {
		return readPasscodeFromReader(stdin)
	}

	fd := int(stdin.Fd())
	fmt.Fprint(os.Stderr, "Node passcode: ")
	first, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read passcode: %w", err)
	}

	fmt.Fprint(os.Stderr, "Confirm passcode: ")
	second, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read passcode confirmation: %w", err)
	}

	pin := strings.TrimSpace(string(first))
	confirm := strings.TrimSpace(string(second))
	if err := matchPasscodeConfirmation(pin, confirm); err != nil {
		return "", err
	}
	return pin, nil
}

// readPasscodeFromReader reads and trims a single line from r. Used for the
// non-interactive (piped stdin) path.
func readPasscodeFromReader(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", fmt.Errorf("read passcode from stdin: %w", err)
		}
		return "", fmt.Errorf("no passcode provided on stdin")
	}
	return strings.TrimSpace(scanner.Text()), nil
}

// matchPasscodeConfirmation reports an error when the two interactive entries
// differ.
func matchPasscodeConfirmation(pin, confirm string) error {
	if pin != confirm {
		return fmt.Errorf("passcodes did not match")
	}
	return nil
}

// cmd/local_passphrase.go
//
// `citadel local passphrase`: the terminal side of local mode's saved logins
// (aceteam#13503 S7). The viewer can create the passphrase too; these commands
// add status, change, lockout recovery and the forgot-my-passphrase path. They
// act only on this user's local state dir and make no network calls.
package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/cobrowseprofile"
	"github.com/aceteam-ai/citadel-cli/internal/localcobrowse"
	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	localPassAck    bool
	localForgetSure bool
)

const localPassphraseDisclosure = `Saved logins are encrypted on this computer with a passphrase that only you
know. Choose four or more random words; a short PIN is refused because it
would not protect your logins if someone copied this computer's disk.
If you forget the passphrase, your saved logins cannot be recovered by
anyone, including AceTeam. This passphrase is for saved logins only: it
gives no access to this computer.`

var localPassphraseCmd = &cobra.Command{
	Use:   "passphrase",
	Short: "Manage the passphrase that protects saved logins in local co-browse",
	Long: `Local co-browse can keep a browser profile's logins between sessions when the
agent starts a session with a profile name. The profile is encrypted on this
computer with your passphrase and unlocked only in the Citadel viewer.

` + localPassphraseDisclosure,
}

var localPassphraseStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show whether a passphrase is set, saved profiles and lockout state",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runLocalPassphraseStatus(localStateDir(), cmd.OutOrStdout())
	},
}

var localPassphraseSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Create the saved-logins passphrase (the viewer can do this too)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		isTTY := term.IsTerminal(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr, localPassphraseDisclosure)
		if err := localConfirmAck(isTTY); err != nil {
			return err
		}
		pass, err := readSecretConfirmed("New passphrase: ", "Repeat the passphrase: ", isTTY)
		if err != nil {
			return err
		}
		return runLocalPassphraseSet(localStateDir(), pass, cmd.OutOrStdout())
	},
}

var localPassphraseChangeCmd = &cobra.Command{
	Use:   "change",
	Short: "Change the saved-logins passphrase (saved logins are kept)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		isTTY := term.IsTerminal(int(os.Stdin.Fd()))
		if err := localConfirmAck(isTTY); err != nil {
			return err
		}
		cur, err := readSecretOnce("Current passphrase: ", isTTY)
		if err != nil {
			return err
		}
		next, err := readSecretConfirmed("New passphrase: ", "Repeat the new passphrase: ", isTTY)
		if err != nil {
			return err
		}
		return runLocalPassphraseChange(localStateDir(), cur, next, cmd.OutOrStdout())
	},
}

var localPassphraseResetLockoutCmd = &cobra.Command{
	Use:   "reset-lockout",
	Short: "Clear the wrong-passphrase lockout for saved logins on this computer",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := cobrowseprofile.NewStore(localStateDir()).Lockout().ResetIfPresent(); err != nil {
			return fmt.Errorf("reset the saved-logins lockout: %w", err)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Saved-logins lockout cleared.")
		return nil
	},
}

var localPassphraseForgetCmd = &cobra.Command{
	Use:   "forget",
	Short: "Delete every saved login and the passphrase (for a forgotten passphrase)",
	Long: `Deletes every saved co-browse profile and the passphrase that protected them,
so you can start again. Use it when you have forgotten the passphrase: without
it the saved logins cannot be opened by anyone. Close your agent apps first;
this refuses while local co-browse is running.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !localForgetSure {
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return errors.New("refusing to delete saved logins non-interactively without --yes")
			}
			fmt.Fprint(os.Stderr, "Type FORGET to delete every saved login and the passphrase: ")
			line, err := bufio.NewReader(os.Stdin).ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			if strings.TrimSpace(line) != "FORGET" {
				return errors.New("not confirmed; nothing was deleted")
			}
		}
		return runLocalPassphraseForget(localStateDir(), cmd.OutOrStdout())
	},
}

func init() {
	for _, c := range []*cobra.Command{localPassphraseSetCmd, localPassphraseChangeCmd} {
		c.Flags().BoolVar(&localPassAck, "ack-data-loss", false,
			"acknowledge that a forgotten passphrase permanently loses saved logins (required when not run in a terminal)")
	}
	localPassphraseForgetCmd.Flags().BoolVar(&localForgetSure, "yes", false, "do not ask for confirmation")
	localPassphraseCmd.AddCommand(localPassphraseStatusCmd, localPassphraseSetCmd, localPassphraseChangeCmd,
		localPassphraseResetLockoutCmd, localPassphraseForgetCmd)
	localCmd.AddCommand(localPassphraseCmd)
}

// localConfirmAck asks for the data-loss acknowledgement on a terminal, or
// requires --ack-data-loss otherwise.
func localConfirmAck(isTTY bool) error {
	if localPassAck {
		return nil
	}
	if !isTTY {
		return errors.New("refusing to set a passphrase non-interactively without --ack-data-loss (a forgotten passphrase loses saved logins)")
	}
	fmt.Fprint(os.Stderr, "Type 'I UNDERSTAND' to confirm a forgotten passphrase cannot be recovered: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if strings.TrimSpace(line) != "I UNDERSTAND" {
		return errors.New("acknowledgement not given; nothing was changed")
	}
	return nil
}

func runLocalPassphraseStatus(dir string, out io.Writer) error {
	s := cobrowseprofile.NewStore(dir)
	st, err := s.State()
	if err != nil {
		return fmt.Errorf("read the saved-logins passphrase: %w", err)
	}
	switch {
	case !st.Configured:
		fmt.Fprintln(out, "Saved-logins passphrase: not set. It is created the first time an agent starts a session with a profile, or with `citadel local passphrase set`.")
	case !st.Strong:
		fmt.Fprintln(out, "Saved-logins passphrase: set, but too short to protect data on disk, so saved logins will not open. Run `citadel local passphrase change`.")
	default:
		fmt.Fprintln(out, "Saved-logins passphrase: set.")
	}
	names, err := s.Profiles()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Fprintln(out, "Saved profiles: none.")
	} else {
		fmt.Fprintf(out, "Saved profiles: %s.\n", strings.Join(names, ", "))
	}
	lo := s.Lockout().Status()
	switch {
	case lo.Unavailable:
		fmt.Fprintln(out, "Lockout: cannot be read, so unlocking is refused. Try `citadel local passphrase reset-lockout`.")
	case lo.LockedOut:
		fmt.Fprintf(out, "Lockout: active until %s after %d wrong passphrases. `citadel local passphrase reset-lockout` clears it.\n",
			lo.LockoutUntil.Local().Format("15:04:05"), lo.FailedAttempts)
	case lo.FailedAttempts > 0:
		fmt.Fprintf(out, "Recent wrong passphrases (last 24h): %d.\n", lo.FailedAttempts)
	}
	return nil
}

func runLocalPassphraseSet(dir, pass string, out io.Writer) error {
	if err := cobrowseprofile.NewStore(dir).SetPassphrase(pass, true); err != nil {
		if errors.Is(err, nodevault.ErrAlreadyConfigured) {
			return errors.New("a passphrase is already set; use `citadel local passphrase change`")
		}
		return friendlyProfileError(err)
	}
	fmt.Fprintln(out, "Saved-logins passphrase set. Unlock saved logins with it in the Citadel viewer.")
	return nil
}

func runLocalPassphraseChange(dir, cur, next string, out io.Writer) error {
	s := cobrowseprofile.NewStore(dir)
	st, err := s.State()
	if err != nil {
		return err
	}
	if !st.Configured {
		return errors.New("no passphrase is set yet; use `citadel local passphrase set`")
	}
	if err := s.ChangePassphrase(cur, next, true); err != nil {
		if errors.Is(err, nodevault.ErrWrongPIN) {
			return errors.New("the current passphrase is not right")
		}
		return friendlyProfileError(err)
	}
	fmt.Fprintln(out, "Saved-logins passphrase changed. Saved logins are kept.")
	return nil
}

func runLocalPassphraseForget(dir string, out io.Writer) error {
	// Refuse while a local co-browse owner runs: it may be about to re-seal a
	// profile. Holding the owner lock for the deletion also keeps a new owner
	// from starting halfway through.
	owner, err := localcobrowse.AcquireOwner(dir)
	if err != nil {
		var oe *localcobrowse.OwnerError
		if errors.As(err, &oe) {
			return errors.New("local co-browse is running; close your agent apps (or their Citadel MCP servers) first")
		}
		return err
	}
	defer owner.Release()
	if err := cobrowseprofile.NewStore(dir).Forget(); err != nil {
		return friendlyProfileError(err)
	}
	fmt.Fprintln(out, "Deleted every saved login and the passphrase. The next profile session asks for a new passphrase.")
	return nil
}

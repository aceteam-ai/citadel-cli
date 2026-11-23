package cmd

import (
	"fmt"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/spf13/cobra"
)

var permissionCmd = &cobra.Command{
	Use:   "permission",
	Short: "Inspect and change this node's local permission policy",
	Long: `Inspect or change the local policy enforced by this node's running worker.

The policy is stored in the enrolled node's machine-wide configuration, so an
interactive command and a system service always address the same node. Local
Shell disable is a kill switch: it is checked before each shell job and is not
changed by a node:exec grant.`,
	RunE: runPermissionStatus,
}

var permissionShellCmd = &cobra.Command{
	Use:   "shell <enable|disable>",
	Short: "Enable or disable remote SHELL_COMMAND execution",
	Args:  cobra.ExactArgs(1),
	RunE:  runPermissionShell,
}

var permissionCobrowsePublicOnlyCmd = &cobra.Command{
	Use:   "cobrowse-public-only <enable|disable>",
	Short: "Limit remote co-browse sessions to public internet addresses",
	Long: `When enabled, every remote co-browse session on this node runs its browser
behind an egress guard: it can reach public websites, but not this computer,
your home or office network (router, NAS, printers), link-local addresses or
other AceTeam Network devices. Takes effect at the next session start.

Remote configuration can turn this on, but only this command turns it off.
Leave it off on a server node that must browse an intranet.`,
	Args: cobra.ExactArgs(1),
	RunE: runPermissionCobrowsePublicOnly,
}

func init() {
	rootCmd.AddCommand(permissionCmd)
	permissionCmd.AddCommand(permissionShellCmd)
	permissionCmd.AddCommand(permissionCobrowsePublicOnlyCmd)
}

func runPermissionStatus(cmd *cobra.Command, args []string) error {
	perms := loadNodePermissions()
	fmt.Fprintf(cmd.OutOrStdout(), "Shell: %s\n", enabledLabel(perms.Shell))
	fmt.Fprintf(cmd.OutOrStdout(), "Passcode: %s\n", enabledLabel(perms.HasPasscode()))
	fmt.Fprintf(cmd.OutOrStdout(), "Co-browse public-only: %s\n", enabledLabel(cobrowsePublicOnlyIn(nodePermissionsDir())))
	return nil
}

func runPermissionCobrowsePublicOnly(cmd *cobra.Command, args []string) error {
	var enabled bool
	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case "enable", "enabled":
		enabled = true
	case "disable", "disabled":
		enabled = false
	default:
		return fmt.Errorf("cobrowse-public-only must be 'enable' or 'disable'")
	}
	if err := setCobrowsePublicOnly(nodePermissionsDir(), enabled); err != nil {
		return err
	}
	if enabled {
		fmt.Fprintln(cmd.OutOrStdout(), "Remote co-browse sessions will reach public internet addresses only. Takes effect at the next session start; running sessions keep their current setting.")
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "Remote co-browse sessions may reach any address this computer can, including your local network. Takes effect at the next session start.")
	}
	return nil
}

func runPermissionShell(cmd *cobra.Command, args []string) error {
	var enabled bool
	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case "enable", "enabled":
		enabled = true
	case "disable", "disabled":
		enabled = false
	default:
		return fmt.Errorf("shell permission must be 'enable' or 'disable'")
	}

	perms, err := setShellPermission(nodePermissionsDir(), enabled)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Shell remote command access %s. Takes effect on the next shell job; no worker restart is needed.\n", enabledLabel(enabled))
	if enabled && !perms.HasPasscode() {
		fmt.Fprintln(cmd.OutOrStdout(), "Shell remains fail-closed until a node passcode or master PIN is configured.")
	}
	return nil
}

func setShellPermission(configDir string, enabled bool) (*config.Permissions, error) {
	perms, err := config.LoadPermissionsStrict(configDir)
	if err != nil {
		return nil, fmt.Errorf("the node permissions file must be readable before this setting can change: %w", err)
	}
	perms.Shell = enabled
	if err := config.SavePermissions(configDir, perms); err != nil {
		return nil, fmt.Errorf("save shell permission: %w", err)
	}
	return perms, nil
}

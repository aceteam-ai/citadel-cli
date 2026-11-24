package cmd

import (
	"fmt"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/localviewer"
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

var permissionFilesCmd = &cobra.Command{
	Use:   "files <enable|disable>",
	Short: "Allow or refuse AceTeam agents reading and uploading files in the workspace",
	Long: `Allow or refuse the Files permission: AceTeam agents may read files in this
node's workspace folder and upload files through co-browse. Takes effect on the
next job; no worker restart is needed. Remote configuration can also change it.`,
	Args: cobra.ExactArgs(1),
	RunE: runPermissionFiles,
}

var permissionSetupPostureCmd = &cobra.Command{
	Use:   "setup-posture disable",
	Short: "Remove the co-browse-only setting a setup-page connect wrote",
	Long: `A PC connected to AceTeam from its setup page keeps a co-browse-only
setting: remote configuration cannot turn anything on, and the worker runs only
co-browse sessions (plus Files or Shell when you allow them here). The setting
stays after Disconnect, so connecting again keeps the PC co-browse only.

'disable' removes that setting. It turns nothing on by itself; the other
permissions keep their current values.`,
	Args: cobra.ExactArgs(1),
	RunE: runPermissionSetupPosture,
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
	permissionCmd.AddCommand(permissionFilesCmd)
	permissionCmd.AddCommand(permissionSetupPostureCmd)
	permissionCmd.AddCommand(permissionCobrowsePublicOnlyCmd)
}

func runPermissionStatus(cmd *cobra.Command, args []string) error {
	perms := loadNodePermissions()
	fmt.Fprintf(cmd.OutOrStdout(), "Shell: %s\n", enabledLabel(perms.Shell))
	fmt.Fprintf(cmd.OutOrStdout(), "Files: %s\n", enabledLabel(perms.Files))
	fmt.Fprintf(cmd.OutOrStdout(), "Co-browse-only setup posture: %s\n", enabledLabel(config.SetupPostureActive(nodePermissionsDir())))
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

// setShellPermission refuses a permissions file it cannot parse rather than
// rewrite it from the defaults (setNodeSurface).
func setShellPermission(configDir string, enabled bool) (*config.Permissions, error) {
	return setNodeSurface(configDir, localviewer.SurfaceShell, enabled, "cli")
}

func runPermissionFiles(cmd *cobra.Command, args []string) error {
	enabled, err := parseEnableDisable("files", args[0])
	if err != nil {
		return err
	}
	if _, err := setNodeSurface(nodePermissionsDir(), localviewer.SurfaceFiles, enabled, "cli"); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Files access %s. Takes effect on the next job; no worker restart is needed.\n", enabledLabel(enabled))
	return nil
}

func runPermissionSetupPosture(cmd *cobra.Command, args []string) error {
	if strings.ToLower(strings.TrimSpace(args[0])) != "disable" {
		return fmt.Errorf("setup-posture only supports 'disable'; the setup page sets it when it connects this PC")
	}
	if err := clearSetupPosture(nodePermissionsDir()); err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), "The co-browse-only setting is removed. Nothing else changed; remote configuration can now change this PC's permissions again.")
	return nil
}

func parseEnableDisable(what, arg string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "enable", "enabled":
		return true, nil
	case "disable", "disabled":
		return false, nil
	}
	return false, fmt.Errorf("%s permission must be 'enable' or 'disable'", what)
}

package cmd

import (
	"errors"
	"fmt"
	"log"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/spf13/cobra"
)

// This command changes local saved intent only. No remote job, page, MCP tool
// or driver calls the writer, and no harness execution is enabled here.
func newPermissionHarnessCommand(dir func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "harness <enable|disable>",
		Short: "Save the independent local harness permission (default off)",
		Long: `Save this node's local harness permission. It is independent of Shell,
console, files and every other permission. The typed harness execution path is
not wired yet: enabling this setting does not start a process or accept a job.
Future execution must also satisfy owner, profile, identity and isolation gates.
Remote enable and the Control Center control are not implemented here.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			enabled, err := parseEnableDisable("harness", args[0])
			if err != nil {
				return err
			}
			if _, err := setHarnessPermission(dir(), enabled); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Harness local setting %s. Execution is not wired; no harness job or process was started.\n", enabledLabel(enabled))
			return nil
		},
	}
}

func setHarnessPermission(dir string, enabled bool) (*config.Permissions, error) {
	p, err := config.LoadPermissionsStrict(dir)
	if err != nil {
		return nil, errors.New("node permission settings are unreadable; no changes saved")
	}
	p.Harness = enabled
	if err := config.SavePermissions(dir, p); err != nil {
		return nil, errors.New("could not save the local harness setting")
	}
	log.Printf("[permission] audit harness=%s actor=local via=cli", onOff(enabled))
	return p, nil
}

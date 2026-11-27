package cmd

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/housekeeping"
	"github.com/spf13/cobra"
)

// Keep the startup exception exact and after run-profile admission. Future
// status/run commands must not inherit an offline exception by subtree name.
func isHousekeepingPlanCommand(cmd *cobra.Command) bool {
	return cmd.CommandPath() == "citadel housekeeping plan"
}

func newHousekeepingCommand(now func() time.Time) *cobra.Command {
	parent := &cobra.Command{
		Use: "housekeeping", Short: "Preview housekeeping policy without changing the node",
	}
	parent.AddCommand(&cobra.Command{
		Use: "plan", Short: "Evaluate a supplied JSON inventory from stdin (dry run only)",
		Long: "Evaluate bounded policy and observation metadata from stdin and print a JSON receipt. This does not scan directories, inspect processes, verify Git state or delete anything. Candidate estimates require native revalidation before any future cleanup.",
		Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			input, err := housekeeping.ReadInput(cmd.InOrStdin())
			if err != nil {
				return err
			}
			receipt, err := housekeeping.Plan(now(), input)
			if err != nil {
				return err
			}
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(receipt); err != nil {
				return errors.New("housekeeping_output_failed")
			}
			return nil
		},
	})
	return parent
}

func init() { rootCmd.AddCommand(newHousekeepingCommand(time.Now)) }

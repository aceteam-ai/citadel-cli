package cmd

import (
	"context"
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/laneinventory"
	"github.com/spf13/cobra"
)

func newLaneInventoryCommand(selected *string, collect func(context.Context, string) (laneinventory.Report, error)) *cobra.Command {
	return &cobra.Command{
		Use: "inventory", Short: "Read bounded lane metadata for this user (no argv, environment or file contents)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := selectedDeclaredNodeDir(cmd, *selected)
			if err != nil {
				return err
			}
			report, err := collect(cmd.Context(), dir)
			if err != nil {
				return laneinventory.ErrUnavailable
			}
			raw, err := laneinventory.Encode(report)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
			return err
		},
	}
}

func collectNativeLaneInventory(ctx context.Context, nodeDir string) (laneinventory.Report, error) {
	return laneinventory.NewNative(nodeDir).Collect(ctx)
}

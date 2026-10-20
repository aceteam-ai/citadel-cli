package cmd

import (
	"encoding/json"
	"errors"
	"time"

	streameval "github.com/aceteam-ai/citadel-cli/internal/stream/eval"
	"github.com/spf13/cobra"
)

func newStreamCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "stream",
		Short: "Evaluate deterministic stream rules offline",
		Long:  "Evaluate reviewed rule packs against bounded local log files. Live stream sources and deployment are not part of this command.",
	}
	var mode, expectedPath string
	var lateness time.Duration
	evaluate := &cobra.Command{
		Use:          "eval <pack> <logs>",
		Short:        "Evaluate a rule pack against local logs without network access",
		Long:         "Read one local log file or the sorted direct .log and .jsonl files in a directory. Each file is an independent scenario. Output contains matches and digests, not raw log lines. Ingest replay uses a fixed UTC epoch and one millisecond per line. Event-time replay uses parsed timestamps and the configured lateness allowance.",
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(command *cobra.Command, args []string) error {
			report, err := streameval.Run(command.Context(), args[0], args[1], streameval.Options{
				TimeMode:        mode,
				AllowedLateness: lateness,
				ExpectedPath:    expectedPath,
			})
			if err != nil {
				return err
			}
			encoder := json.NewEncoder(command.OutOrStdout())
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(report); err != nil {
				return errors.New("stream eval could not write the JSON report")
			}
			if report.Comparison != nil && !report.Comparison.Passed {
				return errors.New("stream eval expectations did not match; see the JSON comparison")
			}
			return nil
		},
	}
	evaluate.Flags().StringVar(&mode, "time", "ingest", "Window time: ingest or event")
	evaluate.Flags().DurationVar(&lateness, "allowed-lateness", 30*time.Second, "Event-time lateness allowance (zero to one hour)")
	evaluate.Flags().StringVar(&expectedPath, "expected", "", "Optional local JSON labels: scenarios mapped to arrays of rule_id and opaque group_key digest")
	command.AddCommand(evaluate)
	return command
}

func init() {
	rootCmd.AddCommand(newStreamCommand())
}

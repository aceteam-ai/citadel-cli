package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/laneinventory"
	"github.com/aceteam-ai/citadel-cli/internal/sessionsearch"
	"github.com/spf13/cobra"
)

type sessionFinder func(context.Context, sessionsearch.Options, []laneinventory.Pane) (sessionsearch.Report, error)

func newSessionFindCommand(selected *string, find sessionFinder, inventory func(context.Context, string) (laneinventory.Report, error)) *cobra.Command {
	var since, until, harness string
	var asJSON bool
	command := &cobra.Command{
		Use: "find <words>", Short: "Search this user's local session prompts and replies without a backend",
		Args: cobra.MinimumNArgs(1), SilenceErrors: true, SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := sessionsearch.Options{Query: strings.Join(args, " "), Harness: harness}
			parse := func(value string) (time.Time, error) {
				if value == "" {
					return time.Time{}, nil
				}
				if t, e := time.Parse(time.RFC3339, value); e == nil {
					return t, nil
				}
				return time.Parse("2006-01-02", value)
			}
			var err error
			opts.Since, err = parse(since)
			if err != nil {
				return sessionsearch.ErrOptions
			}
			opts.Until, err = parse(until)
			if err != nil {
				return sessionsearch.ErrOptions
			}
			dir, err := selectedDeclaredNodeDir(cmd, *selected)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 8*time.Second)
			defer cancel()
			// Content search comes first. Optional placement metadata cannot
			// consume the content scan's deadline or prevent offline results.
			report, err := find(ctx, opts, nil)
			if err != nil {
				return sessionsearch.ErrUnavailable
			}
			if inventory != nil && len(report.Hits) > 0 && ctx.Err() == nil {
				placementCtx, stop := context.WithTimeout(ctx, 2*time.Second)
				placement, placementErr := inventory(placementCtx, dir)
				stop()
				if placementErr == nil {
					sessionsearch.Locate(&report, placement.Panes)
				} else {
					report.Unavailable = append(report.Unavailable, "location")
				}
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(report)
			}
			for _, hit := range report.Hits {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\n", hit.Title, hit.Harness, hit.LastActive.Format(time.RFC3339), hit.State, hit.Location); err != nil {
					return err
				}
				for _, snippet := range hit.Snippets {
					if _, err := fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s\n", snippet.Kind, snippet.Text); err != nil {
						return err
					}
				}
				if hit.Open != "" {
					if _, err := fmt.Fprintf(cmd.OutOrStdout(), "  Open: %s\n", hit.Open); err != nil {
						return err
					}
				}
			}
			if len(report.Hits) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No matching local sessions.")
			}
			if report.Partial || len(report.Unavailable) > 0 {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "Coverage incomplete; some local sources or locations were unavailable.")
			}
			return err
		},
	}
	command.Flags().StringVar(&since, "since", "", "Earliest match (RFC3339 or UTC date, inclusive)")
	command.Flags().StringVar(&until, "until", "", "Latest match (RFC3339 or UTC date, inclusive)")
	command.Flags().StringVar(&harness, "harness", "", "claude or codex (default: both)")
	command.Flags().BoolVar(&asJSON, "json", false, "Print bounded JSON results and coverage")
	return command
}

// cmd/footprints.go
package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/footprint"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/spf13/cobra"
)

var (
	footprintsService  string
	footprintsSince    string
	footprintsIdleOnly bool
)

var footprintsCmd = &cobra.Command{
	Use:   "footprints",
	Short: "Summarize logged per-service resource footprints",
	Long: `Summarize the per-service resource footprint history recorded by the
background sampler (` + "`citadel work`" + ` writes one sample per minute).

For each managed service it reports average/peak RAM, average/peak VRAM,
average/peak CPU, the fraction of samples that were idle, the longest idle
stretch, and the sample count + time window.

The raw rows live in rotated daily CSVs under ~/citadel-node/footprints/
(footprints-YYYY-MM-DD.csv) and are directly DuckDB/pandas-queryable for
ad-hoc analysis, e.g.:

  duckdb -c "SELECT service, avg(rss_mb), max(rss_mb) \
    FROM '~/citadel-node/footprints/*.csv' GROUP BY 1 ORDER BY 2 DESC"

The node-level row is recorded under the service name "_node" and carries
host CPU/RAM plus total GPU utilisation and VRAM used.`,
	Example: `  # Summarize all services over the last hour
  citadel footprints --since 1h

  # One service only
  citadel footprints --service vllm --since 6h

  # Only samples where the node was idle
  citadel footprints --idle-only --since 24h`,
	Run: runFootprints,
}

func init() {
	footprintsCmd.Flags().StringVar(&footprintsService, "service", "", "Restrict to a single service name")
	footprintsCmd.Flags().StringVar(&footprintsSince, "since", "", "Only include samples newer than this duration (e.g. 30m, 1h, 6h)")
	footprintsCmd.Flags().BoolVar(&footprintsIdleOnly, "idle-only", false, "Only include samples where the node was idle")
	rootCmd.AddCommand(footprintsCmd)
}

func runFootprints(cmd *cobra.Command, args []string) {
	nodeDir, err := platform.DefaultNodeDir("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not resolve node dir: %v\n", err)
		os.Exit(1)
	}
	dir := footprint.DefaultDir(nodeDir)

	opts := footprint.QueryOptions{
		Service:  footprintsService,
		IdleOnly: footprintsIdleOnly,
	}
	if footprintsSince != "" {
		since, err := time.ParseDuration(footprintsSince)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid --since %q (use e.g. 30m, 1h, 6h): %v\n", footprintsSince, err)
			os.Exit(1)
		}
		opts.Since = since
	}

	interval, _ := footprint.IntervalFromEnv()
	summaries, err := footprint.Summarize(dir, opts, interval)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not read footprints: %v\n", err)
		os.Exit(1)
	}

	if len(summaries) == 0 {
		fmt.Printf("No footprint samples found in %s\n", dir)
		fmt.Println("The sampler runs under `citadel work`; wait for it to record data,")
		fmt.Println("or check CITADEL_FOOTPRINT_INTERVAL is not set to a non-positive value.")
		return
	}

	printFootprintSummaries(summaries, dir)
}

func printFootprintSummaries(summaries []footprint.ServiceSummary, dir string) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERVICE\tSAMPLES\tAVG RAM\tPEAK RAM\tAVG VRAM\tPEAK VRAM\tAVG CPU\tPEAK CPU\tIDLE %\tLONGEST IDLE\tWINDOW")
	for _, s := range summaries {
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%.0f%%\t%s\t%s\n",
			s.Service,
			s.Samples,
			mb(s.AvgRSSMB),
			mb(s.PeakRSSMB),
			mb(s.AvgVRAMMB),
			mb(s.PeakVRAMMB),
			pct(s.AvgCPUPercent),
			pct(s.PeakCPUPercent),
			s.IdlePercent,
			idleDur(s.LongestIdle),
			window(s.FirstSeen, s.LastSeen),
		)
	}
	w.Flush()
	fmt.Printf("\nRaw CSVs: %s%s*.csv (DuckDB/pandas-queryable)\n", dir, string(filepath.Separator))
}

// mb formats a megabyte value, blanking a zero so "no data" reads as "-".
func mb(v float64) string {
	if v <= 0 {
		return "-"
	}
	if v >= 1024 {
		return fmt.Sprintf("%.1f GB", v/1024)
	}
	return fmt.Sprintf("%.0f MB", v)
}

func pct(v float64) string {
	if v <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", v)
}

func idleDur(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	return d.Round(time.Second).String()
}

func window(first, last time.Time) string {
	if first.IsZero() || last.IsZero() {
		return "-"
	}
	return fmt.Sprintf("%s → %s", first.Local().Format("15:04"), last.Local().Format("15:04"))
}

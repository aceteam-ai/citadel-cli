// cmd/debug.go
/*
Copyright © 2025 AceTeam <dev@aceteam.ai>
*/
package cmd

import (
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/clilog"
	"github.com/spf13/cobra"
)

var debugTailLines int

// debugCmd prints a copy-pasteable debug bundle to stdout. On a headless node
// reached over SSH, copying out of the TUI's clipboard is unreliable, so this
// dumps everything an operator needs to paste into a bug report — version,
// host, the dated log path, and a tail of the always-on activity log — to
// stdout where the terminal can copy it normally.
var debugCmd = &cobra.Command{
	Use:   "debug",
	Short: "Print a copy-pasteable debug bundle (version, host, recent logs)",
	Long: `Print a debug bundle to stdout: citadel version, host/OS, the path to the
date-based append-only log, and the last N lines of activity.

Designed for headless nodes over SSH where the TUI clipboard does not reach
your machine. Pipe or redirect it to share:

  citadel debug                # print to terminal
  citadel debug > debug.txt    # save to a file you can scp
  citadel debug -n 500         # include more log lines`,
	Args: cobra.NoArgs,
	RunE: runDebug,
}

func runDebug(cmd *cobra.Command, args []string) error {
	var sb strings.Builder

	fmt.Fprintln(&sb, "=== citadel debug bundle ===")
	fmt.Fprintf(&sb, "version:  %s\n", Version)
	fmt.Fprintf(&sb, "time:     %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&sb, "os/arch:  %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&sb, "log dir:  %s\n", clilog.Dir())
	fmt.Fprintf(&sb, "log file: %s\n", clilog.Path())
	fmt.Fprintln(&sb)

	fmt.Fprintf(&sb, "--- last %d activity log lines ---\n", debugTailLines)
	tail, err := agentTailLogs(struct {
		Lines int
		Level string
		Grep  string
		Since string
	}{Lines: debugTailLines})
	if err != nil {
		fmt.Fprintf(&sb, "(could not read activity log: %v)\n", err)
	} else if strings.TrimSpace(tail) == "" {
		fmt.Fprintln(&sb, "(no activity logged yet)")
	} else {
		sb.WriteString(tail)
		if !strings.HasSuffix(tail, "\n") {
			sb.WriteByte('\n')
		}
	}

	fmt.Print(sb.String())
	return nil
}

func init() {
	rootCmd.AddCommand(debugCmd)
	debugCmd.Flags().IntVarP(&debugTailLines, "lines", "n", 200, "Number of recent log lines to include")
}

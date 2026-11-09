// cmd/local_audit.go
//
// `citadel local audit`: print or verify the local co-browse audit log
// (aceteam#13503 S5). localaudit owns the format and the chain.
package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/localaudit"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/spf13/cobra"
)

type localAuditOptions struct {
	session string
	limit   int
	since   time.Duration
	json    bool
	verify  bool
}

var localAuditOpts localAuditOptions

var localAuditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Show or verify the local co-browse audit log",
	Long: `Prints the actions agents and you took in local co-browse sessions on this
computer, newest last. Each record is hash-chained to the one before it;
--verify checks the chain and reports the first edited, removed or reordered
record. Typed text is never recorded, only its length.

Records older than 90 days are removed (set CITADEL_LOCAL_AUDIT_RETENTION_DAYS
in the agent app's environment to change it).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := filepath.Join(network.GetNodeConfigDir(), localaudit.DirName)
		return runLocalAudit(dir, localAuditOpts, cmd.OutOrStdout())
	},
}

func init() {
	f := localAuditCmd.Flags()
	f.StringVar(&localAuditOpts.session, "session", "", "only this session id")
	f.IntVar(&localAuditOpts.limit, "limit", 50, "newest N records (0 for all)")
	f.DurationVar(&localAuditOpts.since, "since", 0, "only records newer than this, e.g. 24h")
	f.BoolVar(&localAuditOpts.json, "json", false, "print JSON lines")
	f.BoolVar(&localAuditOpts.verify, "verify", false, "verify the hash chain")
	localCmd.AddCommand(localAuditCmd)
}

func runLocalAudit(dir string, o localAuditOptions, out io.Writer) error {
	if o.verify {
		res, err := localaudit.Verify(dir)
		if err != nil {
			return fmt.Errorf("audit log check FAILED: %w", err)
		}
		if res.Records == 0 {
			fmt.Fprintln(out, "No audit records yet.")
			return nil
		}
		fmt.Fprintf(out, "Audit log OK: %d records, seq %d to %d.\n", res.Records, res.FirstSeq, res.LastSeq)
		if res.Truncated {
			fmt.Fprintln(out, "Older records were removed by the retention period, so the check starts at the oldest kept record.")
		}
		return nil
	}
	f := localaudit.Filter{SessionID: o.session, Limit: o.limit}
	if o.since > 0 {
		f.Since = time.Now().Add(-o.since)
	}
	recs, err := localaudit.Read(dir, f)
	if err != nil {
		return err
	}
	if o.json {
		enc := json.NewEncoder(out)
		for _, r := range recs {
			if err := enc.Encode(r); err != nil {
				return err
			}
		}
		return nil
	}
	if len(recs) == 0 {
		fmt.Fprintln(out, "No audit records yet.")
		return nil
	}
	localaudit.WriteText(out, recs)
	return nil
}

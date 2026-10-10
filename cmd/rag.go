// cmd/rag.go
package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/rag"
	svcports "github.com/aceteam-ai/citadel-cli/services"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	ragModel       string
	ragFilePattern string
	ragTopK        int
	ragJSON        bool
)

var ragCmd = &cobra.Command{
	Use:   "rag",
	Short: "Node-local RAG: index this node's documents and query them locally",
	Long: `Build and search a private semantic index over the documents THIS node holds,
entirely on-box, using the node's self-hosted embedding model (the local TEI
service, gte-multilingual-base, on :8102).

No cloud, no backend round-trip: chunks are embedded locally and stored in a
node-local SQLite index (~/citadel-node/index.db by default). The same index a
running 'citadel work' populates over the fabric is the one these commands read.

  citadel rag index ~/citadel-node/workspace   # index a docs directory
  citadel rag query "how do refunds work?"     # semantic search, local results
  citadel rag status                           # doc/chunk counts + model

Indexing and querying require the local TEI embedding service to be running.
Start it with 'citadel module install tei' if it is not yet up.`,
	// Operational failures (TEI down, empty index) are conditions, not misuse.
	SilenceUsage: true,
}

var ragIndexCmd = &cobra.Command{
	Use:   "index <path>",
	Short: "Index a directory (or file) into the node-local semantic index",
	Long: `Chunk, embed, and store the text files under <path> into the node-local index.

Incremental and idempotent: unchanged files are skipped by content hash, and
files deleted on disk are pruned on re-index. Binary files and noise dirs
(.git, node_modules, .venv, ...) are skipped automatically.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runRAGIndex,
}

var ragQueryCmd = &cobra.Command{
	Use:          "query <text>",
	Short:        "Semantic-search the node-local index and show results with provenance",
	Args:         cobra.MinimumNArgs(1),
	SilenceUsage: true,
	RunE:         runRAGQuery,
}

var ragStatusCmd = &cobra.Command{
	Use:          "status",
	Short:        "Show node-local index status (doc/chunk counts, model, last indexed)",
	SilenceUsage: true,
	RunE:         runRAGStatus,
}

func init() {
	ragCmd.PersistentFlags().StringVar(&ragModel, "model", "", "Embedding model override (default: gte-multilingual-base)")
	ragCmd.PersistentFlags().BoolVar(&ragJSON, "json", false, "Emit machine-readable JSON")
	ragIndexCmd.Flags().StringVar(&ragFilePattern, "pattern", "", "Restrict indexed filenames (glob, e.g. \"*.md\")")
	ragQueryCmd.Flags().IntVar(&ragTopK, "top-k", 10, "Max results to return")

	ragCmd.AddCommand(ragIndexCmd)
	ragCmd.AddCommand(ragQueryCmd)
	ragCmd.AddCommand(ragStatusCmd)
	rootCmd.AddCommand(ragCmd)
}

// newRAGService constructs the local RAG service rooted at the node's workspace,
// resolving the same index.db a running worker uses.
func newRAGService() *rag.Service {
	// Local CLI operator is trusted (has shell access), so allow indexing docs
	// dirs outside the workspace.
	return rag.NewLocal(resolveWorkspaceDir(), ragModel)
}

func runRAGIndex(cmd *cobra.Command, args []string) error {
	svc := newRAGService()
	fmt.Printf("Indexing %s via %s ...\n", args[0], svc.Model())
	// Live, throttled progress line while the walk runs (aceteam#10876 C1). Not
	// in --json mode, where the single JSON result is the only output. Ctrl-C
	// requests a graceful stop: the current file commits and a partial, consistent
	// index remains.
	if !ragJSON {
		svc.SetProgressSink(printRAGProgressLine)
	}
	res, err := svc.Index(cmd.Context(), args[0], ragFilePattern)
	if !ragJSON {
		fmt.Fprint(os.Stderr, "\r\033[K") // clear the live line before the summary
	}
	if err != nil {
		return ragEmbedError(err)
	}
	if ragJSON {
		return printJSON(res)
	}
	status := color.GreenString("OK")
	switch {
	case res.Status == "cancelled":
		status = color.YellowString("CANCELLED")
	case res.FilesFailed > 0:
		status = color.YellowString("PARTIAL")
	}
	fmt.Printf("%s indexed %d file(s), skipped %d, failed %d, pruned %d (%d chunks embedded, dim %d)\n",
		status, res.FilesIndexed, res.FilesSkipped, res.FilesFailed, res.FilesRemoved, res.ChunksEmbedded, res.Dim)
	if res.Status == "cancelled" {
		fmt.Printf("%s\n", color.New(color.Faint).Sprint("cancelled; the partial index is consistent — re-run to resume (unchanged files are skipped)"))
	}
	return nil
}

// printRAGProgressLine renders one throttled progress update as a single
// carriage-returned status line on stderr (so --json stdout stays clean). It
// reads the throttled event map emitted by jobs.NewThrottledProgress.
func printRAGProgressLine(m map[string]any) {
	asInt := func(k string) int {
		switch v := m[k].(type) {
		case int:
			return v
		case float64:
			return int(v)
		}
		return 0
	}
	done, total := asInt("done"), asInt("total")
	line := fmt.Sprintf("  indexing: %d", done)
	if total > 0 {
		line += fmt.Sprintf("/%d (%d%%)", total, asInt("percent"))
	}
	if rate, ok := m["rate"].(float64); ok && rate > 0 {
		line += fmt.Sprintf("  %.1f files/s", rate)
	}
	if eta := asInt("eta_seconds"); eta > 0 {
		line += fmt.Sprintf("  eta %ds", eta)
	}
	if cur, ok := m["current"].(string); ok && cur != "" {
		line += "  " + cur
	}
	fmt.Fprintf(os.Stderr, "\r\033[K%s", line)
}

func runRAGQuery(cmd *cobra.Command, args []string) error {
	query := strings.Join(args, " ")
	svc := newRAGService()
	res, err := svc.Query(cmd.Context(), query, ragTopK)
	if err != nil {
		return ragEmbedError(err)
	}
	if ragJSON {
		return printJSON(res)
	}
	if len(res.Hits) == 0 {
		fmt.Printf("No results. %s\n", color.YellowString("Is anything indexed yet? Try 'citadel rag index <path>'."))
		return nil
	}
	fmt.Printf("%s\n", color.New(color.Faint).Sprintf("%s", res.Provenance))
	for i, h := range res.Hits {
		loc := fmt.Sprintf("%s#%d", filepath.Base(h.Path), h.ChunkIndex)
		fmt.Printf("\n%s  %s  %s\n", color.CyanString("%d.", i+1), color.New(color.Bold).Sprint(loc), color.New(color.Faint).Sprintf("(score %.3f)", h.Score))
		fmt.Printf("   %s\n", h.Text)
	}
	return nil
}

// ragStatusJSON wraps rag.Status with the serving TEI build/device
// (aceteam-ai/citadel-cli#1269) for the --json output. rag.Status is embedded so
// its fields stay at the top level (byte-compatible JSON when the TEI fields are
// absent).
type ragStatusJSON struct {
	rag.Status
	TEIBuild  string `json:"tei_build,omitempty"`
	TEIDevice string `json:"tei_device,omitempty"`
}

// teiServingBuildFn reports the image tag and compute device (cpu/cuda) of the
// running citadel-tei container, or ("","") when it cannot be determined.
// Package var seam so a cmd test never shells out to a container runtime.
var teiServingBuildFn = inspectTEIServingBuild

// inspectTEIServingBuild reads the running citadel-tei container's resolved
// image and parses it into (tag, device).
func inspectTEIServingBuild() (tag, device string) {
	rt := catalog.SelectContainerRuntime()
	out, err := exec.Command(rt.EngineBin, "inspect", "--format", "{{.Config.Image}}", "citadel-"+svcports.TEIServiceName).Output()
	if err != nil {
		return "", ""
	}
	return svcports.ParseTEIServingImage(strings.TrimSpace(string(out)))
}

func runRAGStatus(cmd *cobra.Command, args []string) error {
	svc := newRAGService()
	st, err := svc.Status()
	if err != nil {
		return err
	}
	teiBuild, teiDevice := teiServingBuildFn()
	if ragJSON {
		return printJSON(ragStatusJSON{Status: st, TEIBuild: teiBuild, TEIDevice: teiDevice})
	}
	fmt.Printf("Node-local semantic index\n")
	fmt.Printf("  model:        %s\n", st.Model)
	fmt.Printf("  files:        %d\n", st.Files)
	fmt.Printf("  chunks:       %d\n", st.Chunks)
	last := st.LastIndexed
	if last == "" {
		last = "(never)"
	}
	fmt.Printf("  last indexed: %s\n", last)
	fmt.Printf("  db:           %s\n", st.DBPath)
	if teiBuild != "" {
		fmt.Printf("  tei build:    %s (%s)\n", teiBuild, teiDevice)
	}
	fmt.Printf("  %s\n", color.New(color.Faint).Sprint(st.Provenance))
	return nil
}

// ragEmbedError wraps handler errors with an actionable hint when the local TEI
// embedding service is unreachable — the most common operational failure.
func ragEmbedError(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "TEI") || strings.Contains(msg, "did not become ready") {
		return fmt.Errorf("%w\n\nThe local embedding service (TEI) is not reachable on :8102.\n"+
			"Start it with:  citadel module install tei", err)
	}
	return err
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

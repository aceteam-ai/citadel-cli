// cmd/rag.go
package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/rag"
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
	return rag.New(resolveWorkspaceDir(), ragModel)
}

func runRAGIndex(cmd *cobra.Command, args []string) error {
	svc := newRAGService()
	fmt.Printf("Indexing %s via %s ...\n", args[0], svc.Model())
	res, err := svc.Index(cmd.Context(), args[0], ragFilePattern)
	if err != nil {
		return ragEmbedError(err)
	}
	if ragJSON {
		return printJSON(res)
	}
	fmt.Printf("%s indexed %d file(s), skipped %d, pruned %d (%d chunks embedded, dim %d)\n",
		color.GreenString("OK"), res.FilesIndexed, res.FilesSkipped, res.FilesRemoved, res.ChunksEmbedded, res.Dim)
	return nil
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

func runRAGStatus(cmd *cobra.Command, args []string) error {
	svc := newRAGService()
	st, err := svc.Status()
	if err != nil {
		return err
	}
	if ragJSON {
		return printJSON(st)
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

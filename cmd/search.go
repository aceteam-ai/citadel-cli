// cmd/search.go
package cmd

import (
	"fmt"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/rag"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	searchJSON  bool
	searchTopK  int
	searchRoot  string
	searchModel string
)

// searchResultJSON is ONE result in the `citadel search --json` output. This is
// the stable wrapper contract the desktop client (#617/#618/#619/#570) consumes.
// It is defined here (not aliased to rag.QueryResult) so an internal refactor of
// the rag package cannot silently change the wire shape.
//
//	path        absolute file path of the matched chunk's source file
//	chunk_index 0-based index of the chunk within that file
//	snippet     the matched chunk text, truncated to ~500 bytes
//	score       cosine similarity in [-1, 1], higher is more relevant
type searchResultJSON struct {
	Path       string  `json:"path"`
	ChunkIndex int     `json:"chunk_index"`
	Snippet    string  `json:"snippet"`
	Score      float64 `json:"score"`
}

// searchOutputJSON is the top-level `citadel search --json` document. count may
// be LESS than --top-k when hits were filtered out by the authorized-roots
// check.
type searchOutputJSON struct {
	Query   string             `json:"query"`
	Count   int                `json:"count"`
	Model   string             `json:"model"`
	Results []searchResultJSON `json:"results"`
}

var searchCmd = &cobra.Command{
	Use:   "search <query>",
	Short: "Semantic search over this node's authorized directories (local, private)",
	Long: `Search the files under your authorized root directories by meaning, entirely
on-box, using the node's self-hosted embedding model (local TEI, :8102).

Nothing is searchable until you authorize a root directory:

  citadel search roots add ~/Documents      # authorize a directory
  citadel search index                       # index all authorized roots
  citadel search "quarterly revenue"         # semantic search
  citadel search "quarterly revenue" --json  # machine-readable output

The authorized-roots allowlist is the security boundary: only files under an
authorized root are ever indexed or returned. Indexing and searching require the
local TEI embedding service (start it with 'citadel module install tei').`,
	Args:         cobra.MinimumNArgs(1),
	SilenceUsage: true,
	RunE:         runSearch,
}

var searchIndexCmd = &cobra.Command{
	Use:   "index [path]",
	Short: "Index the authorized roots (or a path under one) into the local index",
	Long: `Chunk, embed, and store files into the node-local semantic index.

With no argument, indexes every authorized root. With a path, indexes that path
(which must resolve under an authorized root). Incremental and idempotent:
unchanged files are skipped and deleted files are pruned.`,
	Args:         cobra.MaximumNArgs(1),
	SilenceUsage: true,
	RunE:         runSearchIndex,
}

var searchRootsCmd = &cobra.Command{
	Use:   "roots",
	Short: "Manage the authorized root directories (the search allowlist)",
	Long:  "View, authorize, or de-authorize the root directories this node may index and search.",
}

var searchRootsListCmd = &cobra.Command{
	Use:          "list",
	Short:        "List authorized root directories",
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runSearchRootsList,
}

var searchRootsAddCmd = &cobra.Command{
	Use:          "add <path>",
	Short:        "Authorize a root directory for indexing and search",
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runSearchRootsAdd,
}

var searchRootsRemoveCmd = &cobra.Command{
	Use:          "remove <path>",
	Short:        "De-authorize a root directory",
	Aliases:      []string{"rm"},
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runSearchRootsRemove,
}

func init() {
	searchCmd.PersistentFlags().StringVar(&searchModel, "model", "", "Embedding model override (default: gte-multilingual-base)")
	searchCmd.Flags().BoolVar(&searchJSON, "json", false, "Emit machine-readable JSON (stable wrapper contract)")
	searchCmd.Flags().IntVar(&searchTopK, "top-k", 10, "Max results to return")
	searchCmd.Flags().StringVar(&searchRoot, "root", "", "Restrict the search to this authorized root")

	searchRootsCmd.AddCommand(searchRootsListCmd)
	searchRootsCmd.AddCommand(searchRootsAddCmd)
	searchRootsCmd.AddCommand(searchRootsRemoveCmd)

	searchCmd.AddCommand(searchIndexCmd)
	searchCmd.AddCommand(searchRootsCmd)
	rootCmd.AddCommand(searchCmd)
}

// activeRoots resolves the roots the current invocation operates over. Normally
// the full authorized allowlist; when --root is set, just that root (which must
// itself resolve under an authorized root, else an error).
func activeRoots() ([]string, error) {
	roots := config.LoadRoots(platform.ConfigDir())
	if len(roots.Roots) == 0 {
		return nil, fmt.Errorf("no authorized roots configured\n\nAuthorize a directory first:  citadel search roots add <path>")
	}
	if searchRoot == "" {
		return roots.Roots, nil
	}
	// --root must be inside the allowlist (equal to, or under, an authorized root).
	resolved, err := jobs.ValidateWithinRoots(roots.Roots, searchRoot)
	if err != nil {
		return nil, fmt.Errorf("--root %q is not within any authorized root: %w", searchRoot, err)
	}
	return []string{resolved}, nil
}

// newSearchService builds a roots-mode rag.Service over the active roots.
func newSearchService() (*rag.Service, error) {
	roots, err := activeRoots()
	if err != nil {
		return nil, err
	}
	return rag.NewWithRoots(roots, resolveWorkspaceDir(), searchModel), nil
}

func runSearch(cmd *cobra.Command, args []string) error {
	svc, err := newSearchService()
	if err != nil {
		return err
	}
	query := strings.Join(args, " ")
	res, err := svc.Query(cmd.Context(), query, searchTopK)
	if err != nil {
		return ragEmbedError(err)
	}

	if searchJSON {
		out := searchOutputJSON{Query: query, Count: len(res.Hits), Model: res.Model, Results: make([]searchResultJSON, 0, len(res.Hits))}
		for _, h := range res.Hits {
			out.Results = append(out.Results, searchResultJSON{Path: h.Path, ChunkIndex: h.ChunkIndex, Snippet: h.Text, Score: h.Score})
		}
		return printJSON(out)
	}

	if len(res.Hits) == 0 {
		fmt.Printf("No results. %s\n", color.YellowString("Is anything indexed yet? Try 'citadel search index'."))
		return nil
	}
	fmt.Printf("%s\n", color.New(color.Faint).Sprint(res.Provenance))
	for i, h := range res.Hits {
		fmt.Printf("\n%s  %s  %s\n", color.CyanString("%d.", i+1), color.New(color.Bold).Sprint(h.Path), color.New(color.Faint).Sprintf("(chunk %d, score %.3f)", h.ChunkIndex, h.Score))
		fmt.Printf("   %s\n", h.Text)
	}
	return nil
}

func runSearchIndex(cmd *cobra.Command, args []string) error {
	roots := config.LoadRoots(platform.ConfigDir())
	if len(roots.Roots) == 0 {
		return fmt.Errorf("no authorized roots configured\n\nAuthorize a directory first:  citadel search roots add <path>")
	}
	svc := rag.NewWithRoots(roots.Roots, resolveWorkspaceDir(), searchModel)

	// Targets: an explicit path (validated against roots by the service) or every
	// authorized root.
	targets := roots.Roots
	if len(args) == 1 {
		targets = []string{args[0]}
	}

	var totalIndexed, totalSkipped, totalRemoved, totalChunks int
	for _, t := range targets {
		fmt.Printf("Indexing %s via %s ...\n", t, svc.Model())
		res, err := svc.Index(cmd.Context(), t, "")
		if err != nil {
			return ragEmbedError(err)
		}
		totalIndexed += res.FilesIndexed
		totalSkipped += res.FilesSkipped
		totalRemoved += res.FilesRemoved
		totalChunks += res.ChunksEmbedded
	}
	fmt.Printf("%s indexed %d file(s), skipped %d, pruned %d (%d chunks embedded)\n",
		color.GreenString("OK"), totalIndexed, totalSkipped, totalRemoved, totalChunks)
	return nil
}

func runSearchRootsList(cmd *cobra.Command, args []string) error {
	roots := config.LoadRoots(platform.ConfigDir())
	if searchJSON {
		return printJSON(roots)
	}
	if len(roots.Roots) == 0 {
		fmt.Printf("No authorized roots. Add one with: %s\n", color.CyanString("citadel search roots add <path>"))
		return nil
	}
	fmt.Println("Authorized roots:")
	for _, r := range roots.Roots {
		fmt.Printf("  - %s\n", r)
	}
	return nil
}

func runSearchRootsAdd(cmd *cobra.Command, args []string) error {
	configDir := platform.ConfigDir()
	roots := config.LoadRoots(configDir)
	added, err := roots.Add(args[0])
	if err != nil {
		return err
	}
	if !added {
		fmt.Printf("%s already authorized\n", args[0])
		return nil
	}
	if err := config.SaveRoots(configDir, roots); err != nil {
		return err
	}
	// Report the normalized form that was actually stored.
	norm, _ := config.NormalizeRoot(args[0])
	fmt.Printf("%s authorized root %s\n", color.GreenString("OK"), norm)
	fmt.Printf("Run %s to index it now.\n", color.CyanString("citadel search index"))
	return nil
}

func runSearchRootsRemove(cmd *cobra.Command, args []string) error {
	configDir := platform.ConfigDir()
	roots := config.LoadRoots(configDir)
	removed, err := roots.Remove(args[0])
	if err != nil {
		return err
	}
	if !removed {
		fmt.Printf("%s was not an authorized root\n", args[0])
		return nil
	}
	if err := config.SaveRoots(configDir, roots); err != nil {
		return err
	}
	norm, _ := config.NormalizeRoot(args[0])
	fmt.Printf("%s de-authorized root %s\n", color.GreenString("OK"), norm)
	fmt.Printf("%s\n", color.New(color.Faint).Sprint("Its chunks remain in the index until the next re-index prunes them; search no longer returns them."))
	return nil
}

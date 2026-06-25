// cmd/module_search.go
//
// `citadel module search <query>` searches the curated central index (#347) and
// prints matching modules with their source and current trust state. The index
// is a YAML file (index.yaml at the catalog repo root, or CITADEL_MODULE_INDEX)
// mapping a module name to a source repo, description, and tags.
//
// Curated entries inform the trusted tier: a curated source is a candidate
// verified publisher, so each result shows whether it is already trusted (cross-
// referenced against the local allowlist via catalog.IsTrusted). The command
// fails soft -- if no index is published yet it prints a friendly hint rather
// than erroring.
//
// Registered in this file's own init() to minimize merge conflicts.
package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var moduleSearchCmd = &cobra.Command{
	Use:   "search [query]",
	Short: "Search the curated module index by name, description, or tag",
	Long: `Search the curated central index for installable modules.

The index maps module names to source repos, descriptions, and tags. Results
show the install source and whether that source is already trusted on this node
(see 'citadel module trust'). Install a result with:

  citadel module install <source>

If no index has been published yet (or the catalog has not been cloned), this
prints a friendly hint instead of an error. Run 'citadel service catalog update'
to fetch the latest catalog (which carries the index).`,
	Args: cobra.MaximumNArgs(1),
	RunE: runModuleSearch,
}

func init() {
	moduleCmd.AddCommand(moduleSearchCmd)
}

// runModuleSearch loads the curated index, matches the query, and prints results
// with source + trust state.
func runModuleSearch(cmd *cobra.Command, args []string) error {
	query := ""
	if len(args) == 1 {
		query = args[0]
	}

	idx, err := catalog.LoadModuleIndex()
	if err != nil {
		return err
	}
	if len(idx.Modules) == 0 {
		fmt.Println("No curated module index is available yet.")
		fmt.Println("  Run 'citadel service catalog update' to fetch the latest catalog (which carries the index),")
		fmt.Println("  or install a module directly: citadel module install <owner/repo | git URL>.")
		return nil
	}

	results := catalog.SearchModuleIndex(idx, query)
	if len(results) == 0 {
		fmt.Printf("No modules matching '%s' in the curated index.\n", query)
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	color.New(color.Bold).Fprintf(w, "MODULE\tSOURCE\tTRUST\tDESCRIPTION\n")
	for _, e := range results {
		trust := color.New(color.FgWhite).Sprint("untrusted")
		// A curated entry is a candidate publisher; cross-reference the local
		// allowlist so the operator sees what is already trusted vs. what would
		// trigger the untrusted-source warning at install time.
		if src, perr := catalog.ParseSource(e.Source); perr == nil && catalog.IsTrusted(src) {
			trust = color.New(color.FgGreen).Sprint("trusted")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.Name, e.Source, trust, truncate(e.Description, 50))
	}
	return nil
}

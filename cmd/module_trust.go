// cmd/module_trust.go
//
// `citadel module trust` manages the local trusted-source allowlist used by the
// module-install flow. Installing a module = `docker compose up` of arbitrary
// compose = root-equivalent (Docker-level) access to the node, so an untrusted
// source triggers a prominent warning at install time. Trusting a source
// suppresses that warning (it does NOT relax the privileged-compose gate, which
// always requires --allow-privileged for Critical findings).
//
// Patterns:
//   - exact "owner/repo"     trusts that GitHub shorthand
//   - "owner/*"              trusts any repo under that owner
//   - a bare host "github.com" / "git.example.com" trusts any source on that host
//
// Catalog sources (first-party) are always trusted (Tier 0) and are not listed.
package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var moduleTrustList bool

var moduleTrustCmd = &cobra.Command{
	Use:   "trust [pattern]",
	Short: "Trust a module source (owner/repo, owner/*, or a host), or list trusted sources with --list",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runModuleTrust,
}

var moduleUntrustCmd = &cobra.Command{
	Use:   "untrust <pattern>",
	Short: "Remove a pattern from the trusted-source allowlist",
	Args:  cobra.ExactArgs(1),
	RunE:  runModuleUntrust,
}

var moduleTrustedCmd = &cobra.Command{
	Use:   "trusted",
	Short: "List the trusted module-source patterns",
	Args:  cobra.NoArgs,
	RunE:  runModuleTrustedList,
}

func init() {
	moduleCmd.AddCommand(moduleTrustCmd)
	moduleCmd.AddCommand(moduleUntrustCmd)
	moduleCmd.AddCommand(moduleTrustedCmd)

	moduleTrustCmd.Flags().BoolVar(&moduleTrustList, "list", false, "List the trusted-source patterns instead of adding one.")
}

// runModuleTrust adds a pattern to the allowlist, or lists patterns with --list.
func runModuleTrust(cmd *cobra.Command, args []string) error {
	if moduleTrustList || len(args) == 0 {
		return runModuleTrustedList(cmd, nil)
	}
	pattern := args[0]
	if err := catalog.AddTrustedSource(pattern); err != nil {
		return fmt.Errorf("failed to trust '%s': %w", pattern, err)
	}
	fmt.Printf("Trusted module source: %s\n", color.CyanString(pattern))
	fmt.Printf("  (stored in %s)\n", catalog.TrustedSourcesPath())
	return nil
}

// runModuleUntrust removes a pattern from the allowlist.
func runModuleUntrust(cmd *cobra.Command, args []string) error {
	pattern := args[0]
	if err := catalog.RemoveTrustedSource(pattern); err != nil {
		return fmt.Errorf("failed to untrust '%s': %w", pattern, err)
	}
	fmt.Printf("Removed trusted module source: %s\n", pattern)
	return nil
}

// runModuleTrustedList prints the trusted-source patterns.
func runModuleTrustedList(cmd *cobra.Command, args []string) error {
	ts, err := catalog.LoadTrustedSources()
	if err != nil {
		return err
	}
	if len(ts.Patterns) == 0 {
		fmt.Println("No trusted module sources. Catalog sources are always trusted (Tier 0).")
		fmt.Println("Add one with: citadel module trust <owner/repo | owner/* | host>")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	color.New(color.Bold).Fprintf(w, "TRUSTED PATTERN\n")
	for _, p := range ts.Patterns {
		fmt.Fprintf(w, "%s\n", p)
	}
	return nil
}

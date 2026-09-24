package cmd

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Command help is built from multiple fields and flag usages. Check the real
// registered command tree without executing commands or contacting a node.
func TestCommandHelpHasNoInternalIssueReferences(t *testing.T) {
	ref := regexp.MustCompile(`#\d+\b`)
	var visit func(*cobra.Command)
	visit = func(c *cobra.Command) {
		copy := strings.Join([]string{c.Short, c.Long, c.Example}, "\n")
		if ref.MatchString(copy) {
			t.Errorf("%s help leaks an internal reference: %s", c.CommandPath(), copy)
		}
		c.Flags().VisitAll(func(f *pflag.Flag) {
			if ref.MatchString(f.Usage) {
				t.Errorf("%s --%s: %s", c.CommandPath(), f.Name, f.Usage)
			}
		})
		for _, child := range c.Commands() {
			visit(child)
		}
	}
	visit(rootCmd)
	if !strings.Contains(passcodeCmd.Long, "without\na passcode set, that surface fails closed") {
		t.Fatal("passcode safety guidance was removed")
	}
}

func TestIdentityOutputHasNoInternalIssueReferences(t *testing.T) {
	var output bytes.Buffer
	renderIdentity(&output, NodeIdentity{})
	if !strings.Contains(output.String(), "not available locally") {
		t.Fatal("missing unavailable identity explanation")
	}
	if regexp.MustCompile(`#\d+\b`).MatchString(output.String()) {
		t.Fatalf("identity copy leaks reference: %s", output.String())
	}
}

func TestLocalToolDescriptionsHaveNoInternalIssueReferences(t *testing.T) {
	ref := regexp.MustCompile(`#\d+\b`)
	for _, tool := range newLocalMCPTools(localMCPDeps{}) {
		if ref.MatchString(tool.Description) {
			t.Errorf("%s description leaks an internal reference: %s", tool.Name, tool.Description)
		}
	}
}

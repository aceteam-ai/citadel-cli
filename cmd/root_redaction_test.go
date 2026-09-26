package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestInvocationSummaryOmitsSuppliedValues(t *testing.T) {
	command := &cobra.Command{Use: "init"}
	command.Flags().String("authkey", "", "")
	command.Flags().String("debug-redis-url", "", "")
	if err := command.Flags().Set("authkey", "private-preauth-key"); err != nil {
		t.Fatal(err)
	}
	if err := command.Flags().Set("debug-redis-url", "redis://name:password@host"); err != nil {
		t.Fatal(err)
	}
	got := invocationSummary(command, []string{"private-positional-token"})
	for _, secret := range []string{"private-preauth-key", "password", "private-positional-token"} {
		if strings.Contains(got, secret) {
			t.Fatalf("invocation summary exposed supplied value: %q", got)
		}
	}
}

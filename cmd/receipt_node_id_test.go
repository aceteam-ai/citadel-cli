package cmd

import "testing"

func TestServingReceiptNodeIDIsThePublishedName(t *testing.T) {
	for _, name := range []string{"ubuntu-gpu", " spaced ", ""} {
		if got := servingReceiptNodeID(name); got != name {
			t.Fatalf("servingReceiptNodeID(%q) = %q, want the published name unchanged", name, got)
		}
	}
}

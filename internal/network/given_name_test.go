package network

import "testing"

func TestCitadelGivenName(t *testing.T) {
	cases := map[string]string{
		"linux-node":         "linux-node-citadel",
		"linux-node-citadel": "linux-node-citadel", // idempotent: never double-suffix
		"":                   "",                   // empty untouched; caller supplies its own default
	}
	for in, want := range cases {
		if got := CitadelGivenName(in); got != want {
			t.Errorf("CitadelGivenName(%q) = %q, want %q", in, got, want)
		}
	}
}

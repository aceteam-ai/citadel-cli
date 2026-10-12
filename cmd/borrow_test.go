package cmd

import "testing"

func TestBorrowServeEnabled_DefaultOff(t *testing.T) {
	t.Setenv("CITADEL_BORROW_SERVE", "")
	if borrowServeEnabled() {
		t.Fatal("borrow serving must be OFF by default (unset env)")
	}
}

func TestBorrowServeEnabled_TruthyVariants(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", " on "} {
		t.Setenv("CITADEL_BORROW_SERVE", v)
		if !borrowServeEnabled() {
			t.Fatalf("CITADEL_BORROW_SERVE=%q should enable serving", v)
		}
	}
}

func TestBorrowServeEnabled_FalseyVariants(t *testing.T) {
	for _, v := range []string{"0", "false", "no", "off", "nonsense"} {
		t.Setenv("CITADEL_BORROW_SERVE", v)
		if borrowServeEnabled() {
			t.Fatalf("CITADEL_BORROW_SERVE=%q should NOT enable serving", v)
		}
	}
}

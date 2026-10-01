package main

import "testing"

func TestRunRejectsUnexpectedPositionals(t *testing.T) {
	t.Parallel()
	if code := run([]string{"extra"}); code != 2 {
		t.Fatalf("run() = %d, want usage exit 2", code)
	}
}

func TestRunHelpSucceeds(t *testing.T) {
	t.Parallel()
	if code := run([]string{"--help"}); code != 0 {
		t.Fatalf("run() = %d, want success", code)
	}
}

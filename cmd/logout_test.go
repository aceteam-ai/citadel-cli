package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestReadLogoutAPIKeyStdinStrictFraming(t *testing.T) {
	for _, tt := range []struct {
		name, input, want string
		bad               bool
	}{
		{name: "line", input: "api_token-123.abc\n", want: "api_token-123.abc"},
		{name: "crlf", input: "api-token\r\n", want: "api-token"},
		{name: "leading NBSP", input: "\u00a0api-token\n", bad: true},
		{name: "trailing NBSP", input: "api-token\u00a0\n", bad: true},
		{name: "leading C1 NEL", input: "\u0085api-token\n", bad: true},
		{name: "trailing C1 NEL", input: "api-token\u0085\n", bad: true},
		{name: "extra newline", input: "api-token\n\n", bad: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readLogoutAPIKeyStdin(strings.NewReader(tt.input), false)
			if (err != nil) != tt.bad || got != tt.want {
				t.Fatalf("got %q, err %v", got, err)
			}
			if err != nil && strings.Contains(err.Error(), tt.input) {
				t.Fatal("error exposed secret input")
			}
		})
	}
}

func TestReadLogoutAPIKeyStdinRefusesTerminal(t *testing.T) {
	if _, err := readLogoutAPIKeyStdin(strings.NewReader("secret"), true); err == nil {
		t.Fatal("terminal API-key input was accepted")
	}
}

func TestStrictDeregistrationFailurePreservesLocalState(t *testing.T) {
	backendErr := errors.New("backend unavailable")
	localCalls := 0
	localLogout := func() error { localCalls++; return nil }

	if err := completeRequiredDeregistration(backendErr, true, localLogout); !errors.Is(err, backendErr) {
		t.Fatalf("strict error = %v", err)
	}
	if localCalls != 0 {
		t.Fatalf("strict failure cleared local state %d time(s)", localCalls)
	}
	if err := completeRequiredDeregistration(backendErr, false, localLogout); err != nil {
		t.Fatalf("best-effort logout failed: %v", err)
	}
	if localCalls != 1 {
		t.Fatalf("best-effort logout calls = %d", localCalls)
	}
}

func TestRequireBackendDeregistrationCannotSkip(t *testing.T) {
	called := false
	call := func(context.Context, string, string) error { called = true; return nil }
	if err := requireBackendDeregistration(context.Background(), "node", "", call); err == nil {
		t.Fatal("missing API key accepted")
	}
	if called {
		t.Fatal("backend called without credential")
	}
	want := errors.New("backend refused")
	if err := requireBackendDeregistration(context.Background(), "node", "key", func(context.Context, string, string) error { return want }); !errors.Is(err, want) {
		t.Fatalf("backend failure not propagated: %v", err)
	}
	if err := requireBackendDeregistration(context.Background(), "node", "key", call); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("successful path skipped backend deregistration")
	}
}

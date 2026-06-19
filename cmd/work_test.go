package cmd

import (
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/update"
)

// TestResolveAutoUpdateEnabled verifies the precedence that lets the web UI /
// `citadel update enable/disable` toggle a node: explicit --auto-update flag >
// CITADEL_AUTO_UPDATE env (on/off) > persisted state > default-off.
func TestResolveAutoUpdateEnabled(t *testing.T) {
	writeState := func(t *testing.T, auto bool) {
		t.Helper()
		if err := update.SaveState(&update.State{AutoUpdate: auto, Channel: "stable"}); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
	}

	cases := []struct {
		name     string
		flag     bool
		env      string // "" means unset
		state    *bool  // nil means no state file
		expected bool
	}{
		{name: "default off when nothing set", expected: false},
		{name: "persisted state enables", state: boolPtr(true), expected: true},
		{name: "persisted state disables", state: boolPtr(false), expected: false},
		{name: "env true overrides disabled state", env: "true", state: boolPtr(false), expected: true},
		{name: "env off overrides enabled state", env: "off", state: boolPtr(true), expected: false},
		{name: "flag wins over env and state", flag: true, env: "off", state: boolPtr(false), expected: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir()) // isolate the on-disk update state
			t.Setenv("CITADEL_AUTO_UPDATE", tc.env)
			orig := workAutoUpdate
			workAutoUpdate = tc.flag
			t.Cleanup(func() { workAutoUpdate = orig })
			if tc.state != nil {
				writeState(t, *tc.state)
			}
			if got := resolveAutoUpdateEnabled(); got != tc.expected {
				t.Errorf("resolveAutoUpdateEnabled() = %v, want %v", got, tc.expected)
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }

func TestShellQueueName(t *testing.T) {
	tests := []struct {
		orgID string
		want  string
	}{
		{
			orgID: "550e8400-e29b-41d4-a716-446655440000",
			want:  "jobs:v1:shell:org_550e8400-e29b-41d4-a716-446655440000",
		},
		{
			orgID: "test-org-id",
			want:  "jobs:v1:shell:org_test-org-id",
		},
		{
			orgID: "",
			want:  "jobs:v1:shell:org_",
		},
	}

	for _, tt := range tests {
		got := shellQueueName(tt.orgID)
		if got != tt.want {
			t.Errorf("shellQueueName(%q) = %q, want %q", tt.orgID, got, tt.want)
		}
	}
}

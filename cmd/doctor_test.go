// cmd/doctor_test.go
package cmd

import "testing"

// TestDoctorCommandRegistered pins that `citadel doctor` (and its `dr` alias)
// resolve to doctorCmd. Cobra silently shadows a duplicate Use/alias rather
// than failing the build, so this is the only thing that would catch a
// collision with another command claiming "doctor" or "dr".
func TestDoctorCommandRegistered(t *testing.T) {
	found := false
	for _, c := range rootCmd.Commands() {
		if c.Use == "doctor" {
			found = true
			if len(c.Aliases) != 1 || c.Aliases[0] != "dr" {
				t.Errorf("doctorCmd.Aliases = %v, want [\"dr\"]", c.Aliases)
			}
			break
		}
	}
	if !found {
		t.Fatal("doctor command not registered on rootCmd")
	}

	resolved, _, err := rootCmd.Find([]string{"doctor"})
	if err != nil || resolved != doctorCmd {
		t.Errorf("rootCmd.Find([\"doctor\"]) = %v, %v, want doctorCmd", resolved, err)
	}
	resolved, _, err = rootCmd.Find([]string{"dr"})
	if err != nil || resolved != doctorCmd {
		t.Errorf("rootCmd.Find([\"dr\"]) = %v, %v, want doctorCmd (alias)", resolved, err)
	}
}

// TestJoinOrNone pins the "(none)" fallback and comma-joining doctorCheckContainerEngine
// relies on to render the "Depends on it" line.
func TestJoinOrNone(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		want  string
	}{
		{"empty", nil, "(none)"},
		{"one", []string{"ollama"}, "ollama"},
		{"many", []string{"ollama", "vllm", "llamacpp"}, "ollama, vllm, llamacpp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := joinOrNone(tt.names); got != tt.want {
				t.Errorf("joinOrNone(%v) = %q, want %q", tt.names, got, tt.want)
			}
		})
	}
}

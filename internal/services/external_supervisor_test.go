// internal/services/external_supervisor_test.go
package services

import (
	"strings"
	"testing"
)

// TestClassifyExternalSupervisor drives the pure decision core with fabricated
// /proc/<pid>/cgroup contents -- never a real /proc read, since this dev box
// runs a live node whose real ollama.service would otherwise be in scope.
func TestClassifyExternalSupervisor(t *testing.T) {
	// cgroup v1 has one line per controller; the "name=systemd" line is
	// authoritative. Pin that the multi-line form resolves the same unit.
	v1Ollama := "12:pids:/system.slice/ollama.service\n" +
		"11:memory:/system.slice/ollama.service\n" +
		"1:name=systemd:/system.slice/ollama.service\n"
	v1Citadel := "1:name=systemd:/system.slice/citadel-worker.service\n"

	cases := []struct {
		name         string
		target       string
		self         string
		wantUnit     string
		wantExternal bool
	}{
		{
			name:         "citadel-started engine shares citadel's own unit",
			target:       "0::/system.slice/citadel-worker.service",
			self:         "0::/system.slice/citadel-worker.service",
			wantExternal: false,
		},
		{
			name:         "host ollama.service seen from inside citadel-worker",
			target:       "0::/system.slice/ollama.service",
			self:         "0::/system.slice/citadel-worker.service",
			wantUnit:     "ollama.service",
			wantExternal: true,
		},
		{
			name:         "host ollama.service seen from a login shell scope",
			target:       "0::/system.slice/ollama.service",
			self:         "0::/user.slice/user-1000.slice/session-3.scope",
			wantUnit:     "ollama.service",
			wantExternal: true,
		},
		{
			// Leaf owner is a .scope even though user@1000.service is higher up
			// the tree. A hand-run binary from a graphical terminal must NOT be
			// misattributed to the user manager (would say "stop user@1000").
			name:         "hand-run process under a graphical terminal scope",
			target:       "0::/user.slice/user-1000.slice/user@1000.service/app.slice/vte-spawn-abc.scope",
			self:         "0::/system.slice/citadel-worker.service",
			wantExternal: false,
		},
		{
			name:         "delegated sub-cgroup resolves up to its service",
			target:       "0::/system.slice/ollama.service/subcg",
			self:         "0::/system.slice/citadel-worker.service",
			wantUnit:     "ollama.service",
			wantExternal: true,
		},
		{
			name:         "cgroup v1 multi-line resolves the systemd hierarchy",
			target:       v1Ollama,
			self:         v1Citadel,
			wantUnit:     "ollama.service",
			wantExternal: true,
		},
		{
			name:         "root cgroup with no owning unit",
			target:       "0::/",
			self:         "0::/system.slice/citadel-worker.service",
			wantExternal: false,
		},
		{
			// Same unit AND both are a bare scope: a citadel run from a shell that
			// itself started a child engine in the same session scope.
			name:         "both in the same session scope",
			target:       "0::/user.slice/user-1000.slice/session-3.scope",
			self:         "0::/user.slice/user-1000.slice/session-3.scope",
			wantExternal: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			unit, external := classifyExternalSupervisor(tc.target, tc.self)
			if external != tc.wantExternal {
				t.Errorf("external = %v, want %v (unit=%q)", external, tc.wantExternal, unit)
			}
			if unit != tc.wantUnit {
				t.Errorf("unit = %q, want %q", unit, tc.wantUnit)
			}
		})
	}
}

func TestExternallyManagedGuidance(t *testing.T) {
	msg := ExternallyManagedGuidance("ollama", "ollama.service")
	for _, want := range []string{
		"ollama is managed outside citadel by host systemd unit ollama.service",
		"sudo systemctl stop ollama",
		"sudo systemctl disable ollama",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("guidance missing %q\n got: %s", want, msg)
		}
	}
	// The command form strips the .service suffix; the identifying phrase keeps
	// the full unit name. Guard against a doubled ".service.service".
	if strings.Contains(msg, "systemctl stop ollama.service") {
		t.Errorf("command form should use the bare unit name, got: %s", msg)
	}
}

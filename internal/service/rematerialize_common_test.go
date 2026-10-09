package service

import (
	"strings"
	"testing"
)

// TestRootRemediationCommand pins citadel-cli#1266 defect 2: the remediation an
// unprivileged refresh prints must actually work under sudo. A bare
// `sudo citadel ...` is "command not found" for a ~/.local/bin install because
// sudo's secure_path strips it, so the command must use an ABSOLUTE path (when
// os.Executable() resolved) or a command-substitution fallback -- never a bare
// `sudo citadel`.
func TestRootRemediationCommand(t *testing.T) {
	t.Run("resolved exe: absolute path, not a bare sudo citadel", func(t *testing.T) {
		exe := "/home/jason/.local/bin/citadel"
		got := rootRemediationCommand(exe)
		if strings.Contains(got, "sudo citadel ") {
			t.Fatalf("remediation uses a bare `sudo citadel` that secure_path breaks: %q", got)
		}
		if !strings.Contains(got, exe) {
			t.Fatalf("remediation should invoke the absolute binary path %q, got %q", exe, got)
		}
		if !strings.HasPrefix(got, "sudo ") {
			t.Fatalf("remediation should run under sudo, got %q", got)
		}
		if !strings.Contains(got, "service refresh-unit") {
			t.Fatalf("remediation should point at the network-free `service refresh-unit`, got %q", got)
		}
	})

	t.Run("unresolved exe: command-substitution fallback, still not bare", func(t *testing.T) {
		got := rootRemediationCommand("")
		if strings.Contains(got, "sudo citadel ") {
			t.Fatalf("fallback remediation is a bare `sudo citadel`: %q", got)
		}
		if !strings.Contains(got, "command -v citadel") {
			t.Fatalf("fallback should resolve citadel via command substitution, got %q", got)
		}
		if !strings.Contains(got, "service refresh-unit") {
			t.Fatalf("fallback should point at `service refresh-unit`, got %q", got)
		}
	})
}

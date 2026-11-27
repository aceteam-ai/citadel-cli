package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/memory"
)

func TestMemoryAuthorizationNeeded_FailsClosedOnRotation(t *testing.T) {
	cfg := &memory.Config{APIKey: "act_existing"}
	if needed, err := memoryAuthorizationNeeded(cfg, true); err == nil || needed {
		t.Fatalf("force rotation should fail closed, needed=%v err=%v", needed, err)
	}
	if needed, err := memoryAuthorizationNeeded(cfg, false); err != nil || needed {
		t.Fatalf("existing key should be reused, needed=%v err=%v", needed, err)
	}
	if needed, err := memoryAuthorizationNeeded(nil, false); err != nil || !needed {
		t.Fatalf("fresh install should authorize, needed=%v err=%v", needed, err)
	}
}

func TestQuoteHookArg_SuppressesShellExpansion(t *testing.T) {
	got, err := quoteHookArg(`/tmp/Citadel $(touch BAD) '$HOME'/citadel`)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") || !strings.Contains(got, `'"'"'`) {
			t.Fatalf("path was not strongly POSIX-quoted: %q", got)
		}
		if strings.HasPrefix(got, `"`) {
			t.Fatalf("double quoting permits shell substitution: %q", got)
		}
	}
}

func TestQuoteHookArgForOS_RejectsWindowsExpansion(t *testing.T) {
	for _, path := range []string{`C:\%TEMP%\citadel.exe`, `C:\!USER!\citadel.exe`, "C:\\bad\nname.exe"} {
		if _, err := quoteHookArgForOS("windows", path); err == nil {
			t.Fatalf("unsafe Windows path accepted: %q", path)
		}
	}
	got, err := quoteHookArgForOS("windows", `C:\Program Files\Citadel\citadel.exe`)
	if err != nil || got != `"C:\Program Files\Citadel\citadel.exe"` {
		t.Fatalf("safe Windows path: got=%q err=%v", got, err)
	}
}

func TestMemoryElevatedUserCheck(t *testing.T) {
	if err := memoryElevatedUserCheck(false, ""); err != nil {
		t.Fatalf("ordinary user rejected: %v", err)
	}
	if err := memoryElevatedUserCheck(true, "owner"); err == nil || !strings.Contains(err.Error(), "without sudo") {
		t.Fatalf("sudo user mismatch not rejected clearly: %v", err)
	}
	if err := memoryElevatedUserCheck(true, ""); err == nil {
		t.Fatal("root execution accepted")
	}
}

func TestMemoryCaptureSecretCheck_FailsClosedWithoutRewriting(t *testing.T) {
	for _, note := range []string{
		"deploy used API_KEY=abcdefghijklmnopqrstuvwx",
		"material:\n-----BEGIN OPENSSH PRIVATE KEY-----\nredacted body",
	} {
		err := memoryCaptureSecretCheck(note)
		if err == nil {
			t.Fatalf("secret-bearing note accepted: %q", note)
		}
		if strings.Contains(err.Error(), "abcdefghijklmnopqrstuvwx") || strings.Contains(err.Error(), "BEGIN OPENSSH") {
			t.Fatalf("error leaked matched content: %v", err)
		}
	}
	if err := memoryCaptureSecretCheck("Decided to use a bounded retry loop."); err != nil {
		t.Fatalf("ordinary prose rejected: %v", err)
	}
}

func TestUninstallMemory_RequiresRemoteRevocationConfirmation(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, ".citadel-cli")
	if err := memory.Save(configDir, &memory.Config{APIKey: "act_existing"}); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.WriteMCPServer(memory.ClaudeJSONPath(home), memory.MCPServerName, "/bin/citadel", []string{"--no-auto-update", "mcp", "--memory-config"}); err != nil {
		t.Fatal(err)
	}
	if err := uninstallMemory(home, configDir, false); err == nil {
		t.Fatal("uninstall should refuse while revocation is unconfirmed")
	}
	if _, err := os.Stat(memory.ConfigPath(configDir)); err != nil {
		t.Fatalf("failed uninstall removed only local credential: %v", err)
	}

	if err := uninstallMemory(home, configDir, true); err != nil {
		t.Fatalf("confirmed uninstall: %v", err)
	}
	if _, err := os.Stat(memory.ConfigPath(configDir)); !os.IsNotExist(err) {
		t.Fatalf("confirmed uninstall retained credential: %v", err)
	}
}

func TestCheckCaptureScope(t *testing.T) {
	for _, tc := range []struct {
		scope string
		allow bool
		ok    bool
	}{
		{"", false, false}, {"  ", true, false},
		{"global", false, false}, {"Global", false, false}, {"global", true, true},
		{"agent:", false, false}, {"Agent:  ", false, false}, {"agent:", true, true},
		{"agent:hermes", false, true}, {"my-project", false, true},
	} {
		err := checkCaptureScope(tc.scope, tc.allow)
		if (err == nil) != tc.ok {
			t.Errorf("checkCaptureScope(%q,%v) err=%v, want ok=%v", tc.scope, tc.allow, err, tc.ok)
		}
	}
}

func captureHookCommands(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(memory.ClaudeSettingsPath(home))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(b)
}

func TestWireClaudeCode_NoCaptureHookWithoutScope(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := wireCaptureHook(memory.ClaudeSettingsPath(home), "/bin/citadel", "", false); err != nil {
		t.Fatal(err)
	}
	if got := captureHookCommands(t, home); strings.Contains(got, memory.CaptureMarker) {
		t.Fatalf("capture hook installed without a scope: %s", got)
	}
	if _, err := wireCaptureHook(memory.ClaudeSettingsPath(home), "/bin/citadel", "global", false); err == nil {
		t.Fatal("global capture accepted without opt-in")
	}
	if got := captureHookCommands(t, home); strings.Contains(got, memory.CaptureMarker) {
		t.Fatal("capture hook installed for refused global scope")
	}
}

func TestWireClaudeCode_ScopedHookAndLegacyRemoval(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := wireCaptureHook(memory.ClaudeSettingsPath(home), "/bin/citadel", "proj", false); err != nil {
		t.Fatal(err)
	}
	got := captureHookCommands(t, home)
	if !strings.Contains(got, memory.CaptureMarker) || !strings.Contains(got, "--scope") || strings.Contains(got, "allow-global") {
		t.Fatalf("scoped hook wrong: %s", got)
	}
	// Reinstalling without a scope removes the hook (fail closed for old installs).
	if _, err := wireCaptureHook(memory.ClaudeSettingsPath(home), "/bin/citadel", "", false); err != nil {
		t.Fatal(err)
	}
	if got := captureHookCommands(t, home); strings.Contains(got, memory.CaptureMarker) {
		t.Fatalf("capture hook survived scope-less reinstall: %s", got)
	}
}

func TestCheckCaptureScope_RefusesOldUnscopedInstall(t *testing.T) {
	// An old install runs the hook with no --scope. The network-level refusal
	// is pinned by memory.CaptureNote's own empty-scope test.
	if err := checkCaptureScope("", false); err == nil {
		t.Fatal("empty scope must be refused before any write")
	}
}

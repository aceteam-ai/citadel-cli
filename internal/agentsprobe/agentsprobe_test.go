package agentsprobe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"bare semver", "1.2.3\n", "1.2.3"},
		{"prefixed", "claude-code 1.2.3 (build abc)\n", "1.2.3"},
		{"major-minor only", "codex-cli v2.5\n", "2.5"},
		{"multiline uses first line", "gemini-cli 0.9.1\nsome extra line\n", "0.9.1"},
		{"no version-shaped token falls back to trimmed line", "  unknown-format  \n", "unknown-format"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseVersion(tt.raw); got != tt.want {
				t.Errorf("parseVersion(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestJSONFileNonEmptyObjectState(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "missing.json")
	if got := jsonFileNonEmptyObjectState(missing); got != AuthStateNo {
		t.Errorf("missing file: got %q, want %q", got, AuthStateNo)
	}

	emptyObj := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(emptyObj, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := jsonFileNonEmptyObjectState(emptyObj); got != AuthStateNo {
		t.Errorf("empty object: got %q, want %q", got, AuthStateNo)
	}

	malformed := filepath.Join(dir, "malformed.json")
	if err := os.WriteFile(malformed, []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := jsonFileNonEmptyObjectState(malformed); got != AuthStateUnknown {
		t.Errorf("malformed json: got %q, want %q", got, AuthStateUnknown)
	}

	notAnObject := filepath.Join(dir, "array.json")
	if err := os.WriteFile(notAnObject, []byte(`[1,2,3]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := jsonFileNonEmptyObjectState(notAnObject); got != AuthStateUnknown {
		t.Errorf("json array (not an object): got %q, want %q", got, AuthStateUnknown)
	}

	populated := filepath.Join(dir, "creds.json")
	if err := os.WriteFile(populated, []byte(`{"accessToken":"sk-does-not-matter"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := jsonFileNonEmptyObjectState(populated); got != AuthStateAuthed {
		t.Errorf("populated object: got %q, want %q", got, AuthStateAuthed)
	}

	if runtime.GOOS != "windows" {
		unreadable := filepath.Join(dir, "noperm.json")
		if err := os.WriteFile(unreadable, []byte(`{"accessToken":"x"}`), 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })
		if os.Getuid() == 0 {
			t.Skip("running as root: permission bits are not enforced")
		}
		if got := jsonFileNonEmptyObjectState(unreadable); got != AuthStateUnknown {
			t.Errorf("unreadable file: got %q, want %q", got, AuthStateUnknown)
		}
	}
}

func TestOpencodeAuthStateAlwaysUnknown(t *testing.T) {
	// Any home dir, populated or not: opencode's credential layout is not
	// confidently known, so this must never claim authed/unauthenticated.
	if got := opencodeAuthState(t.TempDir()); got != AuthStateUnknown {
		t.Errorf("opencodeAuthState = %q, want %q", got, AuthStateUnknown)
	}
}

// fakeVendorBinary writes an executable shell script named `name` into dir
// that prints versionOutput and exits 0 on any arguments (so `--version`
// works without a real vendor CLI installed).
func fakeVendorBinary(t *testing.T, dir, name, versionOutput string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake PATH binaries are POSIX shell scripts; not exercised on windows")
	}
	scriptPath := filepath.Join(dir, name)
	script := fmt.Sprintf("#!/bin/sh\necho %q\n", versionOutput)
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestProbe_DetectsInstalledAndAuthedViaFakePATH(t *testing.T) {
	binDir := t.TempDir()
	fakeVendorBinary(t, binDir, "claude", "1.2.3")
	fakeVendorBinary(t, binDir, "codex", "0.9.0")
	// gemini and opencode deliberately absent from the fake PATH.

	homeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(homeDir, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeDir, ".claude", ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"sk-fake"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// codex home dir left with no auth.json: expect unauthenticated.

	t.Setenv("PATH", binDir)
	t.Setenv("HOME", homeDir)
	// os.UserHomeDir on non-windows reads $HOME.

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	agents := Probe(ctx)
	byName := make(map[string]VendorAgent, len(agents))
	for _, a := range agents {
		byName[a.Name] = a
	}

	claude, ok := byName["claude"]
	if !ok {
		t.Fatal("expected a claude entry")
	}
	if !claude.Installed {
		t.Error("claude: expected Installed=true")
	}
	if claude.Version != "1.2.3" {
		t.Errorf("claude: Version = %q, want %q", claude.Version, "1.2.3")
	}
	if claude.Authed != AuthStateAuthed {
		t.Errorf("claude: Authed = %q, want %q", claude.Authed, AuthStateAuthed)
	}
	if claude.AdapterClass != "claude-code-hooks" {
		t.Errorf("claude: AdapterClass = %q, want %q", claude.AdapterClass, "claude-code-hooks")
	}

	codex, ok := byName["codex"]
	if !ok {
		t.Fatal("expected a codex entry")
	}
	if !codex.Installed {
		t.Error("codex: expected Installed=true")
	}
	if codex.Authed != AuthStateNo {
		t.Errorf("codex: Authed = %q, want %q", codex.Authed, AuthStateNo)
	}

	gemini, ok := byName["gemini"]
	if !ok {
		t.Fatal("expected a gemini entry even when not installed")
	}
	if gemini.Installed {
		t.Error("gemini: expected Installed=false (absent from fake PATH)")
	}
	if gemini.Authed != "" {
		t.Errorf("gemini: Authed = %q, want empty (not meaningful when not installed)", gemini.Authed)
	}
	if gemini.Version != "" {
		t.Errorf("gemini: Version = %q, want empty (not installed)", gemini.Version)
	}

	opencode, ok := byName["opencode"]
	if !ok {
		t.Fatal("expected an opencode entry even when not installed")
	}
	if opencode.Installed {
		t.Error("opencode: expected Installed=false")
	}
}

func TestProbe_NoVendorsOnPATH(t *testing.T) {
	emptyBinDir := t.TempDir()
	t.Setenv("PATH", emptyBinDir)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	agents := Probe(ctx)
	if len(agents) != len(vendorSpecs) {
		t.Fatalf("Probe() returned %d entries, want %d (one per vendor spec, present or not)", len(agents), len(vendorSpecs))
	}
	for _, a := range agents {
		if a.Installed {
			t.Errorf("%s: expected Installed=false with an empty PATH", a.Name)
		}
		if a.Version != "" {
			t.Errorf("%s: expected empty Version with an empty PATH", a.Name)
		}
	}
}

package agentsprobe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

	// The permission-denied case lives in its own subtest so its t.Skip on a
	// root runner (where 0o000 bits are not enforced) does not skip the prior
	// assertions in this test.
	t.Run("permission denied", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("unix permission bits are not enforced on windows")
		}
		if os.Getuid() == 0 {
			t.Skip("running as root: permission bits are not enforced")
		}
		unreadable := filepath.Join(t.TempDir(), "noperm.json")
		if err := os.WriteFile(unreadable, []byte(`{"accessToken":"x"}`), 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })
		if got := jsonFileNonEmptyObjectState(unreadable); got != AuthStateUnknown {
			t.Errorf("unreadable file: got %q, want %q", got, AuthStateUnknown)
		}
	})
}

func TestCredentialFileState(t *testing.T) {
	dir := t.TempDir()

	absent := filepath.Join(dir, "absent.json")

	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	populated := filepath.Join(dir, "creds.json")
	if err := os.WriteFile(populated, []byte(`{"accessToken":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	malformed := filepath.Join(dir, "malformed.json")
	if err := os.WriteFile(malformed, []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}

	const relocationVar = "CITADEL_TEST_RELOCATION_DIR"

	tests := []struct {
		name   string
		path   string
		vars   []string
		goos   string
		setVar bool
		want   AuthState
	}{
		{"absent linux no var → confident no", absent, []string{relocationVar}, "linux", false, AuthStateNo},
		{"absent linux relocation var set → unknown", absent, []string{relocationVar}, "linux", true, AuthStateUnknown},
		{"absent darwin → unknown (keychain)", absent, nil, "darwin", false, AuthStateUnknown},
		{"absent windows → unknown (layout)", absent, nil, "windows", false, AuthStateUnknown},
		{"populated linux → authed regardless of var", populated, []string{relocationVar}, "linux", true, AuthStateAuthed},
		{"populated darwin → authed (file wins)", populated, nil, "darwin", false, AuthStateAuthed},
		{"empty object linux no var → confident no", empty, nil, "linux", false, AuthStateNo},
		{"empty object darwin → unknown (ambiguous)", empty, nil, "darwin", false, AuthStateUnknown},
		{"malformed → unknown regardless of platform", malformed, nil, "linux", false, AuthStateUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Ensure the relocation var is unset by default, then set it only
			// for the case under test (t.Setenv restores on cleanup).
			t.Setenv(relocationVar, "")
			if tt.setVar {
				t.Setenv(relocationVar, filepath.Join(dir, "elsewhere"))
			} else {
				_ = os.Unsetenv(relocationVar)
			}
			if got := credentialFileState(tt.path, tt.vars, tt.goos); got != tt.want {
				t.Errorf("credentialFileState(goos=%q, setVar=%v) = %q, want %q", tt.goos, tt.setVar, got, tt.want)
			}
		})
	}
}

func TestResolveTargetUser(t *testing.T) {
	const procPath = "/usr/bin:/bin"
	lookup := func(homes map[string]string) func(string) (string, error) {
		return func(u string) (string, error) {
			if h, ok := homes[u]; ok {
				return h, nil
			}
			return "", fmt.Errorf("unknown user %q", u)
		}
	}

	t.Run("SUDO_USER set resolves to that user's home", func(t *testing.T) {
		opts, err := resolveTargetUser(
			"alice",
			lookup(map[string]string{"alice": "/home/alice"}),
			func() (string, error) { return "/root", nil },
			procPath,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if opts.HomeDir != "/home/alice" {
			t.Errorf("HomeDir = %q, want /home/alice", opts.HomeDir)
		}
		// PATH must prepend the user-local bin dirs AND retain the process PATH.
		for _, want := range []string{"/home/alice/.npm-global/bin", "/home/alice/.local/bin", "/home/alice/bin", procPath} {
			if !strings.Contains(opts.PathEnv, want) {
				t.Errorf("PathEnv %q missing %q", opts.PathEnv, want)
			}
		}
	})

	t.Run("SUDO_USER unset falls back to process home", func(t *testing.T) {
		opts, err := resolveTargetUser(
			"",
			lookup(nil), // must not be consulted
			func() (string, error) { return "/home/proc", nil },
			procPath,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if opts.HomeDir != "/home/proc" {
			t.Errorf("HomeDir = %q, want /home/proc", opts.HomeDir)
		}
	})

	t.Run("SUDO_USER=root treated as unset (matches resolveConfigDir)", func(t *testing.T) {
		opts, err := resolveTargetUser(
			"root",
			lookup(nil),
			func() (string, error) { return "/root", nil },
			procPath,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if opts.HomeDir != "/root" {
			t.Errorf("HomeDir = %q, want /root", opts.HomeDir)
		}
	})

	t.Run("unresolvable SUDO_USER is an honest error, not a guess", func(t *testing.T) {
		_, err := resolveTargetUser(
			"ghost",
			lookup(map[string]string{"alice": "/home/alice"}),
			func() (string, error) { return "/root", nil },
			procPath,
		)
		if err == nil {
			t.Fatal("expected an error for an unresolvable SUDO_USER, got nil")
		}
	})

	t.Run("process home failure is an honest error", func(t *testing.T) {
		_, err := resolveTargetUser(
			"",
			lookup(nil),
			func() (string, error) { return "", fmt.Errorf("no home") },
			procPath,
		)
		if err == nil {
			t.Fatal("expected an error when process home cannot resolve, got nil")
		}
	})
}

func TestLookPath_OverrideWalk(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("override walk uses POSIX executable-bit semantics")
	}
	// dir1 has a non-executable file and a subdir shadowing the name; dir2 has
	// the real executable. lookPath must skip the first two and find dir2's.
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir1, "notexec"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir1, "claude"), 0o755); err != nil {
		t.Fatal(err) // a directory named like the binary must be skipped
	}
	realBin := filepath.Join(dir2, "claude")
	if err := os.WriteFile(realBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	pathEnv := strings.Join([]string{"", dir1, dir2}, string(os.PathListSeparator))
	got, err := lookPath("claude", pathEnv)
	if err != nil {
		t.Fatalf("lookPath override: unexpected error: %v", err)
	}
	if got != realBin {
		t.Errorf("lookPath = %q, want %q", got, realBin)
	}

	if _, err := lookPath("does-not-exist", pathEnv); err == nil {
		t.Error("lookPath for a missing binary: expected error, got nil")
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

	// A zero Options uses the process HOME/PATH set above (the operator path).
	agents := Probe(ctx, Options{})
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

	agents := Probe(ctx, Options{})
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

// TestProbe_ExplicitOptionsDetect proves the S2 shape works: explicit
// HomeDir/PathEnv are honored WITHOUT relying on the process environment.
func TestProbe_ExplicitOptionsDetect(t *testing.T) {
	binDir := t.TempDir()
	fakeVendorBinary(t, binDir, "claude", "1.2.3")

	homeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(homeDir, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeDir, ".claude", ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"sk-fake"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Point the PROCESS env somewhere that has NOTHING, to prove the explicit
	// Options — not the process env — are what drive detection.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	agents := Probe(ctx, Options{HomeDir: homeDir, PathEnv: binDir})
	var claude VendorAgent
	for _, a := range agents {
		if a.Name == "claude" {
			claude = a
		}
	}
	if !claude.Installed {
		t.Error("claude: expected Installed=true from the explicit PathEnv")
	}
	if claude.Version != "1.2.3" {
		t.Errorf("claude: Version = %q, want 1.2.3", claude.Version)
	}
	if claude.Authed != AuthStateAuthed {
		t.Errorf("claude: Authed = %q, want %q (from explicit HomeDir)", claude.Authed, AuthStateAuthed)
	}
}

// TestProbe_OptionsOverrideReplacesProcessEnv proves the override REPLACES the
// process environment rather than merging with it: even though the process
// HOME/PATH have the binary and credentials, an Options pointing at empty dirs
// must find nothing. This is the exact worker-vs-operator divergence #1005 is
// about, verified in the direction that matters (no accidental fallthrough to
// the process env).
func TestProbe_OptionsOverrideReplacesProcessEnv(t *testing.T) {
	procBinDir := t.TempDir()
	fakeVendorBinary(t, procBinDir, "claude", "1.2.3")

	procHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(procHome, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procHome, ".claude", ".credentials.json"), []byte(`{"accessToken":"sk-fake"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", procBinDir)
	t.Setenv("HOME", procHome)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Override with empty dirs: the binary and creds in the PROCESS env must
	// NOT leak through.
	agents := Probe(ctx, Options{HomeDir: t.TempDir(), PathEnv: t.TempDir()})
	for _, a := range agents {
		if a.Name != "claude" {
			continue
		}
		if a.Installed {
			t.Error("claude: Installed=true — override PathEnv leaked to the process PATH")
		}
	}
}

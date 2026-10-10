package update

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	runtimedebug "runtime/debug"
	"strings"
	"testing"
)

func TestNormalizeExactVersionStrictIdentityAndPrecedence(t *testing.T) {
	valid := map[string]string{
		"1.2.3":                "v1.2.3",
		"v1.2.3-rc.10+linux.1": "v1.2.3-rc.10+linux.1",
	}
	for input, want := range valid {
		got, err := NormalizeExactVersion(input)
		if err != nil || got != want {
			t.Fatalf("NormalizeExactVersion(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "v", "V1.2.3", "vv1.2.3", "1.2", "01.2.3", " 1.2.3", "1.2.3\n", strings.Repeat("1", 129)} {
		if _, err := NormalizeExactVersion(input); !IsDeterministicUpdateError(err) {
			t.Errorf("NormalizeExactVersion(%q) error = %v, want deterministic refusal", input, err)
		}
	}
	if !ExactVersionIdentityEqual("1.2.3+one", "v1.2.3+one") {
		t.Fatal("optional v should not alter exact identity")
	}
	if ExactVersionIdentityEqual("v1.2.3+one", "v1.2.3+two") {
		t.Fatal("build metadata must remain part of exact identity")
	}
	if got, err := CompareReleaseVersions("v1.2.3+two", "v1.2.3+one"); err != nil || got != TargetEqual {
		t.Fatalf("metadata-different precedence = %v, %v; want equal", got, err)
	}
	if got, err := CompareReleaseVersions("v1.2.3-rc.10", "v1.2.3-rc.2"); err != nil || got != TargetNewer {
		t.Fatalf("numeric prerelease precedence = %v, %v; want newer", got, err)
	}
}

func TestParseExecutableBuildInfoModuleDevTrimpathAndBounds(t *testing.T) {
	base := func() *runtimedebug.BuildInfo {
		return &runtimedebug.BuildInfo{
			Path:     citadelMainPackage,
			Main:     runtimedebug.Module{Path: "github.com/aceteam-ai/citadel-cli", Version: "(devel)"},
			Settings: []runtimedebug.BuildSetting{{Key: "GOOS", Value: runtime.GOOS}, {Key: "GOARCH", Value: runtime.GOARCH}},
		}
	}
	t.Run("development", func(t *testing.T) {
		got, err := parseExecutableBuildInfo(base())
		if err != nil || got.Version != "dev" || got.Source != VersionSourceDevelopment {
			t.Fatalf("metadata = %#v, %v", got, err)
		}
	})
	t.Run("module", func(t *testing.T) {
		info := base()
		info.Main.Version = "v2.7.0"
		got, err := parseExecutableBuildInfo(info)
		if err != nil || got.Version != "v2.7.0" || got.Source != VersionSourceModule {
			t.Fatalf("metadata = %#v, %v", got, err)
		}
	})
	t.Run("trimpath ambiguous without observable stamp", func(t *testing.T) {
		info := base()
		info.Settings = append(info.Settings, runtimedebug.BuildSetting{Key: "-trimpath", Value: "true"})
		if _, err := parseExecutableBuildInfo(info); err == nil {
			t.Fatal("trimpath ambiguity accepted")
		}
	})
	t.Run("trimpath with exact stamp", func(t *testing.T) {
		info := base()
		info.Settings = append(info.Settings,
			runtimedebug.BuildSetting{Key: "-trimpath", Value: "true"},
			runtimedebug.BuildSetting{Key: "-ldflags", Value: "-X " + citadelVersionSymbol + "=v2.8.0"},
		)
		got, err := parseExecutableBuildInfo(info)
		if err != nil || got.Version != "v2.8.0" || got.Source != VersionSourceLinkerX {
			t.Fatalf("metadata = %#v, %v", got, err)
		}
	})
	t.Run("duplicate setting", func(t *testing.T) {
		info := base()
		info.Settings = append(info.Settings, runtimedebug.BuildSetting{Key: "GOOS", Value: runtime.GOOS})
		if _, err := parseExecutableBuildInfo(info); err == nil {
			t.Fatal("duplicate build setting accepted")
		}
	})
	t.Run("setting count bound", func(t *testing.T) {
		info := base()
		for i := len(info.Settings); i <= maxBuildSettings; i++ {
			info.Settings = append(info.Settings, runtimedebug.BuildSetting{Key: strings.Repeat("x", i+1), Value: "x"})
		}
		if _, err := parseExecutableBuildInfo(info); err == nil {
			t.Fatal("oversized settings list accepted")
		}
	})
	t.Run("selected version bound", func(t *testing.T) {
		info := base()
		info.Settings = append(info.Settings, runtimedebug.BuildSetting{Key: "-ldflags", Value: "-X " + citadelVersionSymbol + "=" + strings.Repeat("1", maxExactVersionBytes+1)})
		if _, err := parseExecutableBuildInfo(info); err == nil {
			t.Fatal("oversized selected version accepted")
		}
	})
}

func TestValidateExecutableMetadataRejectsPlatformHeaderAndBuildMode(t *testing.T) {
	goodHeader := []byte("\x7fELF")
	if runtime.GOOS == "darwin" {
		goodHeader = []byte("\xfe\xed\xfa\xcf")
	} else if runtime.GOOS == "windows" {
		goodHeader = []byte("MZ\x00\x00")
	}
	good := ExecutableVersion{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, buildMode: "exe"}
	if err := validateExecutableMetadata(goodHeader, good); err != nil {
		t.Fatalf("valid metadata rejected: %v", err)
	}
	bad := good
	bad.GOARCH = "not-this-arch"
	if err := validateExecutableMetadata(goodHeader, bad); err == nil {
		t.Fatal("foreign GOARCH accepted")
	}
	bad = good
	bad.buildMode = "plugin"
	if err := validateExecutableMetadata(goodHeader, bad); err == nil {
		t.Fatal("plugin build mode accepted")
	}
	if err := validateExecutableMetadata([]byte("NOPE"), good); err == nil {
		t.Fatal("foreign executable header accepted")
	}
}

func TestReadExecutableVersionNeverExecutesCandidate(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "executed")
	path := filepath.Join(dir, "citadel")
	script := "#!/bin/sh\ntouch " + marker + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadExecutableVersion(path); err == nil {
		t.Fatal("non-Go sentinel unexpectedly accepted")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("candidate was executed: %v", err)
	}
}

func TestSplitGoQuotedFieldsMatchesGoCommandRules(t *testing.T) {
	got, err := splitGoQuotedFields(`-s -w -X 'github.com/aceteam-ai/citadel-cli/cmd.version=v2.3.4'`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-s", "-w", "-X", "github.com/aceteam-ai/citadel-cli/cmd.version=v2.3.4"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("fields = %#v, want %#v", got, want)
	}
	got, err = splitGoQuotedFields(`-X=github.com/aceteam-ai/citadel-cli/cmd.version=v2.3.4 "two words"x`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[1] != "two words" || got[2] != "x" {
		t.Fatalf("quotes only at field start: %#v", got)
	}
	if _, err := splitGoQuotedFields(`'unterminated`); err == nil {
		t.Fatal("unterminated quote accepted")
	}
}

func TestLinkerVersionOfficialFormsAndBounds(t *testing.T) {
	forms := []string{
		`-s -w -X github.com/aceteam-ai/citadel-cli/cmd.version=v2.174.0`,
		`-s -w -X=github.com/aceteam-ai/citadel-cli/cmd.version=v2.174.0`,
		`-s -w -X 'github.com/aceteam-ai/citadel-cli/cmd.version=v2.174.0'`,
	}
	for _, form := range forms {
		got, found, err := linkerVersion(form)
		if err != nil || !found || got != "v2.174.0" {
			t.Errorf("linkerVersion(%q) = %q, %v, %v", form, got, found, err)
		}
	}
	duplicate := `-X github.com/aceteam-ai/citadel-cli/cmd.version=v1.0.0 -X=github.com/aceteam-ai/citadel-cli/cmd.version=v2.0.0`
	if _, _, err := linkerVersion(duplicate); err == nil {
		t.Fatal("duplicate exact version stamps accepted")
	}
	if _, _, err := linkerVersion(strings.Repeat("x", maxRecordedLDFlagsBytes+1)); err == nil {
		t.Fatal("oversized recorded linker flags accepted")
	}
}

func TestSanitizedMetadataErrorIsBoundedAndOpaque(t *testing.T) {
	err := sanitizedMetadataError(strings.Repeat("p", 400), errors.New("secret"))
	if len(err.Error()) > maxJobMetadataError || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe metadata error: %q", err)
	}
}

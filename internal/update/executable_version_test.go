package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	runtimedebug "runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	fixtureBuildMu    sync.Mutex
	fixtureBuildCount = make(map[string]int)
)

func buildCitadelFixture(t *testing.T, output, version string) {
	t.Helper()
	fixtureBuildMu.Lock()
	defer fixtureBuildMu.Unlock()

	cached := fixtureCachePath(version)
	if _, err := os.Stat(cached); errors.Is(err, os.ErrNotExist) {
		building := cached + ".building"
		_ = os.Remove(building)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		cmd := exec.CommandContext(ctx, "go", "build", "-modcacherw", "-buildvcs=false", "-o", building, "-ldflags", "-s -w -X github.com/aceteam-ai/citadel-cli/cmd.version="+version, "./cmd/citadel")
		cmd.Dir = filepath.Join("..", "..")
		data, buildErr := cmd.CombinedOutput()
		cancel()
		if buildErr != nil {
			_ = os.Remove(building)
			t.Fatalf("compile-only Citadel fixture %s: %v\n%s", version, buildErr, data)
		}
		if err := os.Chmod(building, 0o755); err != nil {
			_ = os.Remove(building)
			t.Fatalf("make compile-only Citadel fixture executable: %v", err)
		}
		if err := os.Rename(building, cached); err != nil {
			_ = os.Remove(building)
			t.Fatalf("publish compile-only Citadel fixture: %v", err)
		}
		fixtureBuildCount[version]++
	} else if err != nil {
		t.Fatalf("stat cached compile-only Citadel fixture: %v", err)
	}
	if err := copyCitadelFixture(cached, output); err != nil {
		t.Fatalf("copy compile-only Citadel fixture %s: %v", version, err)
	}
}

func fixtureCachePath(version string) string {
	sum := sha256.Sum256([]byte(version))
	cacheName := "citadel-" + hex.EncodeToString(sum[:])
	if runtime.GOOS == "windows" {
		cacheName += ".exe"
	}
	return filepath.Join(fixtureCacheDir, cacheName)
}

func copyCitadelFixture(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(destination, 0o755)
}

func TestBuildCitadelFixtureCachesVersionAndCopiesIndependentFiles(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	third := filepath.Join(dir, "third")
	if runtime.GOOS == "windows" {
		first += ".exe"
		second += ".exe"
		third += ".exe"
	}
	const version = "v9.9.9-fixture-cache"

	fixtureBuildMu.Lock()
	if err := os.Remove(fixtureCachePath(version)); err != nil && !errors.Is(err, os.ErrNotExist) {
		fixtureBuildMu.Unlock()
		t.Fatal(err)
	}
	delete(fixtureBuildCount, version)
	fixtureBuildMu.Unlock()
	buildCitadelFixture(t, first, version)
	fixtureBuildMu.Lock()
	afterCold := fixtureBuildCount[version]
	fixtureBuildMu.Unlock()
	t.Setenv("HOME", t.TempDir())
	buildCitadelFixture(t, second, version)
	fixtureBuildMu.Lock()
	afterWarm := fixtureBuildCount[version]
	fixtureBuildMu.Unlock()
	if afterCold != 1 || afterWarm != afterCold {
		t.Fatalf("fixture compiles cold/warm = %d/%d, want one cold compile and warm reuse", afterCold, afterWarm)
	}
	firstInfo, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(second)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(firstInfo, secondInfo) {
		t.Fatal("fixture destinations share one file identity")
	}
	originalSHA, err := fixtureFileSHA256(second)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(first, 1); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	buildCitadelFixture(t, third, version)
	fixtureBuildMu.Lock()
	afterMutatedCopy := fixtureBuildCount[version]
	fixtureBuildMu.Unlock()
	if afterMutatedCopy != afterWarm {
		t.Fatalf("mutating one destination caused a recompile: warm/mutated = %d/%d", afterWarm, afterMutatedCopy)
	}
	thirdSHA, err := fixtureFileSHA256(third)
	if err != nil {
		t.Fatal(err)
	}
	if thirdSHA != originalSHA {
		t.Fatalf("cached fixture changed after destination mutation: %x != %x", thirdSHA, originalSHA)
	}
	thirdInfo, err := os.Stat(third)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(firstInfo, thirdInfo) || os.SameFile(secondInfo, thirdInfo) {
		t.Fatal("warm fixture destination shares another file identity")
	}
	for _, path := range []string{second, third} {
		metadata, err := ReadExecutableVersion(path)
		if err != nil || metadata.Version != version {
			t.Fatalf("fixture %s metadata = %#v, %v", path, metadata, err)
		}
	}
}

func fixtureFileSHA256(path string) ([sha256.Size]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return [sha256.Size]byte{}, err
	}
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

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
	t.Run("empty main version", func(t *testing.T) {
		info := base()
		info.Main.Version = ""
		if _, err := parseExecutableBuildInfo(info); err == nil {
			t.Fatal("missing module provenance accepted as development build")
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

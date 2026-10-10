package update

import (
	"debug/buildinfo"
	"errors"
	"fmt"
	"io"
	"runtime"
	runtimedebug "runtime/debug"
	"strings"
	"unicode"

	semver "github.com/Masterminds/semver/v3"
)

const (
	maxExactVersionBytes    = 128
	maxRecordedLDFlagsBytes = 64 << 10
	maxBuildSettings        = 256
	maxJobMetadataError     = 256
	citadelMainPackage      = "github.com/aceteam-ai/citadel-cli/cmd/citadel"
	citadelVersionSymbol    = "github.com/aceteam-ai/citadel-cli/cmd.version"
)

// VersionSource describes which non-executing build-info field supplied the
// version. It is provenance, not an authenticity statement.
type VersionSource uint8

const (
	VersionSourceLinkerX VersionSource = iota + 1
	VersionSourceModule
	VersionSourceDevelopment
)

// ExecutableVersion is the bounded metadata needed to make an update decision.
type ExecutableVersion struct {
	Version string
	GOOS    string
	GOARCH  string
	Source  VersionSource
	// buildMode is retained only for install compatibility checks.
	buildMode string
}

// VersionRelation compares the requested/candidate release to the installed
// release by SemVer precedence. Build metadata does not affect precedence.
type VersionRelation int

const (
	TargetOlder VersionRelation = -1
	TargetEqual VersionRelation = 0
	TargetNewer VersionRelation = 1
)

// DeterministicUpdateError marks an input/metadata refusal which retrying the
// same exact job cannot repair. Callers use it for terminal job disposition.
type DeterministicUpdateError struct {
	kind string
	err  error
}

func (e *DeterministicUpdateError) Error() string { return e.err.Error() }
func (e *DeterministicUpdateError) Unwrap() error { return e.err }

func deterministicUpdateError(kind, format string, args ...any) error {
	return &DeterministicUpdateError{kind: kind, err: fmt.Errorf(format, args...)}
}

func IsDeterministicUpdateError(err error) bool {
	var target *DeterministicUpdateError
	return errors.As(err, &target)
}

// NormalizeExactVersion validates the deliberately strict exact-target grammar
// and returns the one canonical release-tag spelling used for lookup/identity.
func NormalizeExactVersion(raw string) (string, error) {
	if len(raw) == 0 || len(raw) > maxExactVersionBytes {
		return "", deterministicUpdateError("invalid_target", "invalid exact update target")
	}
	if strings.TrimSpace(raw) != raw || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return "", deterministicUpdateError("invalid_target", "invalid exact update target")
	}
	plain := raw
	if strings.HasPrefix(plain, "v") {
		plain = plain[1:]
	}
	if plain == "" || strings.HasPrefix(plain, "v") {
		return "", deterministicUpdateError("invalid_target", "invalid exact update target")
	}
	if _, err := semver.StrictNewVersion(plain); err != nil {
		return "", deterministicUpdateError("invalid_target", "invalid exact update target")
	}
	return "v" + plain, nil
}

// ExactVersionIdentityEqual compares the complete canonical SemVer identity,
// including prerelease and build metadata; only the optional leading v differs.
func ExactVersionIdentityEqual(a, b string) bool {
	ca, err := NormalizeExactVersion(a)
	if err != nil {
		return false
	}
	cb, err := NormalizeExactVersion(b)
	return err == nil && ca == cb
}

// CompareReleaseVersions compares target against installed by SemVer precedence.
func CompareReleaseVersions(target, installed string) (VersionRelation, error) {
	ct, err := NormalizeExactVersion(target)
	if err != nil {
		return 0, err
	}
	ci, err := NormalizeExactVersion(installed)
	if err != nil {
		return 0, deterministicUpdateError("unknown_installed", "cannot verify installed Citadel release version; refusing exact update")
	}
	tv, _ := semver.StrictNewVersion(strings.TrimPrefix(ct, "v"))
	iv, _ := semver.StrictNewVersion(strings.TrimPrefix(ci, "v"))
	switch tv.Compare(iv) {
	case -1:
		return TargetOlder, nil
	case 1:
		return TargetNewer, nil
	default:
		return TargetEqual, nil
	}
}

// ReadExecutableVersion reads Citadel build metadata without executing path.
func ReadExecutableVersion(path string) (ExecutableVersion, error) {
	f, err := openRegularNoFollow(path)
	if err != nil {
		return ExecutableVersion{}, sanitizedMetadataError("open executable metadata", err)
	}
	defer f.Close()
	return readExecutableVersion(f)
}

func readExecutableVersion(r io.ReaderAt) (ExecutableVersion, error) {
	bi, err := buildinfo.Read(r)
	if err != nil {
		return ExecutableVersion{}, sanitizedMetadataError("read Go build metadata", err)
	}
	return parseExecutableBuildInfo(bi)
}

func parseExecutableBuildInfo(bi *runtimedebug.BuildInfo) (ExecutableVersion, error) {
	if bi == nil {
		return ExecutableVersion{}, fmt.Errorf("executable build metadata is missing")
	}
	if bi.Path != citadelMainPackage {
		return ExecutableVersion{}, fmt.Errorf("executable is not a Citadel CLI main package")
	}
	if len(bi.Settings) > maxBuildSettings {
		return ExecutableVersion{}, fmt.Errorf("executable build metadata has too many settings")
	}

	settings := make(map[string]string, len(bi.Settings))
	for _, setting := range bi.Settings {
		if _, exists := settings[setting.Key]; exists {
			return ExecutableVersion{}, fmt.Errorf("executable build metadata has duplicate settings")
		}
		settings[setting.Key] = setting.Value
	}

	stamp, found, err := linkerVersion(settings["-ldflags"])
	if err != nil {
		return ExecutableVersion{}, err
	}
	goos, goarch := settings["GOOS"], settings["GOARCH"]
	if !validBuildSettingWord(goos) || !validBuildSettingWord(goarch) {
		return ExecutableVersion{}, fmt.Errorf("executable platform metadata is invalid")
	}
	result := ExecutableVersion{GOOS: goos, GOARCH: goarch, buildMode: settings["-buildmode"]}
	if found {
		if len(stamp) == 0 || len(stamp) > maxExactVersionBytes {
			return ExecutableVersion{}, fmt.Errorf("recorded Citadel version stamp is out of bounds")
		}
		result.Version = stamp
		result.Source = VersionSourceLinkerX
		return result, nil
	}
	if settings["-trimpath"] == "true" {
		return ExecutableVersion{}, fmt.Errorf("executable version provenance is ambiguous under trimpath")
	}
	if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		if len(bi.Main.Version) > maxExactVersionBytes {
			return ExecutableVersion{}, fmt.Errorf("module version is out of bounds")
		}
		result.Version = bi.Main.Version
		result.Source = VersionSourceModule
		return result, nil
	}
	result.Version = "dev"
	result.Source = VersionSourceDevelopment
	return result, nil
}

func linkerVersion(raw string) (string, bool, error) {
	if len(raw) > maxRecordedLDFlagsBytes {
		return "", false, fmt.Errorf("recorded linker flags exceed the metadata limit")
	}
	if raw == "" {
		return "", false, nil
	}
	fields, err := splitGoQuotedFields(raw)
	if err != nil {
		return "", false, fmt.Errorf("malformed recorded linker flags")
	}
	var version string
	found := false
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		var assignment string
		switch {
		case field == "-X":
			i++
			if i >= len(fields) {
				return "", false, fmt.Errorf("malformed recorded linker flags")
			}
			assignment = fields[i]
		case strings.HasPrefix(field, "-X="):
			assignment = strings.TrimPrefix(field, "-X=")
		default:
			continue
		}
		name, value, ok := strings.Cut(assignment, "=")
		if !ok || name == "" {
			return "", false, fmt.Errorf("malformed recorded linker flags")
		}
		if name != citadelVersionSymbol {
			continue
		}
		if found || value == "" {
			return "", false, fmt.Errorf("ambiguous recorded Citadel version stamp")
		}
		found = true
		version = value
	}
	return version, found, nil
}

// splitGoQuotedFields implements the Go command's whitespace/single-quote/
// double-quote field rules without invoking a shell.
func splitGoQuotedFields(s string) ([]string, error) {
	var fields []string
	for len(s) > 0 {
		for len(s) > 0 && isGoQuotedSpace(s[0]) {
			s = s[1:]
		}
		if len(s) == 0 {
			break
		}
		if s[0] == '\'' || s[0] == '"' {
			quote := s[0]
			s = s[1:]
			i := strings.IndexByte(s, quote)
			if i < 0 {
				return nil, fmt.Errorf("unterminated quoted string")
			}
			fields = append(fields, s[:i])
			s = s[i+1:]
			continue
		}
		i := 0
		for i < len(s) && !isGoQuotedSpace(s[i]) {
			i++
		}
		fields = append(fields, s[:i])
		s = s[i:]
	}
	return fields, nil
}

func isGoQuotedSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func validateExecutableForInstall(r io.ReaderAt) (ExecutableVersion, error) {
	var header [4]byte
	if _, err := r.ReadAt(header[:], 0); err != nil {
		return ExecutableVersion{}, fmt.Errorf("read executable header: %w", err)
	}
	meta, err := readExecutableVersion(r)
	if err != nil {
		return ExecutableVersion{}, err
	}
	if err := validateExecutableMetadata(header[:], meta); err != nil {
		return ExecutableVersion{}, err
	}
	return meta, nil
}

func validateExecutableMetadata(header []byte, meta ExecutableVersion) error {
	if !headerMatchesGOOS(header, runtime.GOOS) {
		return fmt.Errorf("candidate executable format is incompatible with this platform")
	}
	if meta.GOOS != runtime.GOOS || meta.GOARCH != runtime.GOARCH {
		return fmt.Errorf("candidate platform is incompatible with this Citadel build")
	}
	if meta.buildMode != "" && meta.buildMode != "exe" && meta.buildMode != "pie" {
		return fmt.Errorf("candidate executable build mode is incompatible")
	}
	return nil
}

func validBuildSettingWord(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

func headerMatchesGOOS(header []byte, goos string) bool {
	if len(header) < 4 {
		return false
	}
	switch goos {
	case "linux":
		return string(header[:4]) == "\x7fELF"
	case "windows":
		return header[0] == 'M' && header[1] == 'Z'
	case "darwin":
		magic := string(header[:4])
		return magic == "\xfe\xed\xfa\xce" || magic == "\xce\xfa\xed\xfe" || magic == "\xfe\xed\xfa\xcf" || magic == "\xcf\xfa\xed\xfe" || magic == "\xca\xfe\xba\xbe" || magic == "\xbe\xba\xfe\xca"
	default:
		return false
	}
}

func sanitizedMetadataError(prefix string, err error) error {
	msg := prefix
	if err != nil {
		msg += ": metadata unavailable"
	}
	if len(msg) > maxJobMetadataError {
		msg = msg[:maxJobMetadataError]
	}
	return fmt.Errorf("%s", msg)
}

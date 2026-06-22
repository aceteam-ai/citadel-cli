// internal/jobs/build_handlers.go
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// buildTimeout caps a single build invocation. Real iOS/Android builds can take
// several minutes; the cap protects the worker from a hung toolchain.
const buildTimeout = 30 * time.Minute

// runBuildCommand executes name with args in workspace (when non-empty) and
// returns combined stdout/stderr plus the error. It is the single exec path for
// all build handlers so platform gating and timeout behaviour stay consistent.
//
// It is a variable (not a plain function) so tests can stub the exec layer and
// exercise the handlers' result/error shaping without a real toolchain.
var runBuildCommand = func(workspace, name string, args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if workspace != "" {
		cmd.Dir = workspace
	}
	return cmd.CombinedOutput()
}

// buildResult is the structured JSON returned by every build handler. The
// adapter wraps this string under {"output": ...}; the coordinator pulls any
// produced artifact by reference via FILE_READ_BYTES using artifact_path.
type buildResult struct {
	Tool         string `json:"tool"`
	Command      string `json:"command"`
	Output       string `json:"output"`
	ArtifactPath string `json:"artifact_path,omitempty"`
}

func marshalBuildResult(r buildResult) ([]byte, error) {
	return json.Marshal(r)
}

// nonEmptyField returns the trimmed value of the named payload field, or an
// error naming the field when it is missing or blank.
func nonEmptyField(payload map[string]string, field string) (string, error) {
	v, ok := payload[field]
	if !ok {
		return "", fmt.Errorf("job payload missing %q field", field)
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", fmt.Errorf("job payload field %q is empty", field)
	}
	return v, nil
}

// splitTasks splits a whitespace-separated task/argument list, dropping blanks.
func splitTasks(s string) []string {
	fields := strings.Fields(s)
	return fields
}

// IOSBuildHandler handles IOS_BUILD jobs by invoking xcodebuild. It is gated to
// darwin at runtime; on other platforms Execute returns a structured error so
// the binary still compiles and the job fails cleanly instead of panicking.
type IOSBuildHandler struct {
	WorkspaceDir string
}

// NewIOSBuildHandler creates an IOSBuildHandler rooted at workspace.
func NewIOSBuildHandler(workspace string) *IOSBuildHandler {
	return &IOSBuildHandler{WorkspaceDir: workspace}
}

// Code-sign style values accepted in the IOS_BUILD payload's "code_sign_style"
// field. They map to xcodebuild's CODE_SIGN_STYLE build setting.
const (
	codeSignStyleManual    = "manual"
	codeSignStyleAutomatic = "automatic"
)

// iosSigning holds the optional signing parameters parsed from an IOS_BUILD
// payload. signed reports whether any signing was requested; when false the
// builders fall back to unsigned/simulator behaviour.
type iosSigning struct {
	signed              bool
	style               string // "manual" or "automatic"
	teamID              string
	identity            string // CODE_SIGN_IDENTITY, e.g. "Apple Distribution"
	provisioningProfile string // PROVISIONING_PROFILE_SPECIFIER (name or UUID)
}

// parseIOSSigning extracts and validates the signing parameters from the
// payload. Signing is considered requested when any of team_id, signing_identity,
// provisioning_profile, or code_sign_style is present.
//
// Validation rules:
//   - code_sign_style, when present, must be "manual" or "automatic"; when
//     absent it defaults to "manual" (the only style that needs explicit
//     identity/profile wiring on a CI node).
//   - manual signing requires a team_id (Apple requires a DEVELOPMENT_TEAM for
//     manual provisioning).
func parseIOSSigning(payload map[string]string) (iosSigning, error) {
	var s iosSigning
	s.teamID = strings.TrimSpace(payload["team_id"])
	s.identity = strings.TrimSpace(payload["signing_identity"])
	s.provisioningProfile = strings.TrimSpace(payload["provisioning_profile"])
	style := strings.ToLower(strings.TrimSpace(payload["code_sign_style"]))

	if s.teamID == "" && s.identity == "" && s.provisioningProfile == "" && style == "" {
		// No signing parameters supplied: unsigned/simulator build.
		return s, nil
	}
	s.signed = true

	switch style {
	case "":
		s.style = codeSignStyleManual
	case codeSignStyleManual, codeSignStyleAutomatic:
		s.style = style
	default:
		return iosSigning{}, fmt.Errorf("invalid code_sign_style %q: must be %q or %q", style, codeSignStyleManual, codeSignStyleAutomatic)
	}

	if s.style == codeSignStyleManual && s.teamID == "" {
		return iosSigning{}, fmt.Errorf("manual code signing requires a team_id")
	}

	return s, nil
}

// signingBuildSettings returns the xcodebuild build-setting arguments (KEY=VALUE
// tokens) implied by the signing configuration. For an unsigned build it returns
// CODE_SIGNING_ALLOWED=NO so simulator/compile-only builds never trip on a
// missing identity. For a signed build it emits CODE_SIGN_STYLE plus whichever of
// DEVELOPMENT_TEAM / CODE_SIGN_IDENTITY / PROVISIONING_PROFILE_SPECIFIER were
// provided.
func (s iosSigning) signingBuildSettings() []string {
	if !s.signed {
		return []string{"CODE_SIGNING_ALLOWED=NO"}
	}

	var settings []string
	settings = append(settings, "CODE_SIGN_STYLE="+capitalizeStyle(s.style))
	if s.teamID != "" {
		settings = append(settings, "DEVELOPMENT_TEAM="+s.teamID)
	}
	if s.identity != "" {
		settings = append(settings, "CODE_SIGN_IDENTITY="+s.identity)
	}
	if s.provisioningProfile != "" {
		settings = append(settings, "PROVISIONING_PROFILE_SPECIFIER="+s.provisioningProfile)
	}
	return settings
}

// capitalizeStyle maps the lowercase style token to xcodebuild's expected
// CODE_SIGN_STYLE value ("Manual" / "Automatic").
func capitalizeStyle(style string) string {
	switch style {
	case codeSignStyleManual:
		return "Manual"
	case codeSignStyleAutomatic:
		return "Automatic"
	default:
		return style
	}
}

// buildIOSArgs constructs the xcodebuild argument vector from the payload.
//
// Payload fields (all strings via nexus.Job):
//   - scheme (required): the Xcode scheme to build
//   - configuration (optional): e.g. "Debug" / "Release"
//   - destination (optional): xcodebuild -destination value
//   - workspace_file (optional): path to a .xcworkspace
//   - project (optional): path to a .xcodeproj
//   - sdk (optional): e.g. "iphonesimulator"
//   - derived_data_path (optional): -derivedDataPath value
//   - action (optional): build action(s), default "build"
//
// Signing fields (all optional; see parseIOSSigning):
//   - team_id, signing_identity, provisioning_profile, code_sign_style
//
// When no signing fields are present the build is unsigned
// (CODE_SIGNING_ALLOWED=NO). When manual signing with an automatic style is
// requested, -allowProvisioningUpdates is added so Xcode can manage profiles.
func buildIOSArgs(payload map[string]string) ([]string, error) {
	scheme, err := nonEmptyField(payload, "scheme")
	if err != nil {
		return nil, err
	}

	signing, err := parseIOSSigning(payload)
	if err != nil {
		return nil, err
	}

	var args []string
	if ws := strings.TrimSpace(payload["workspace_file"]); ws != "" {
		args = append(args, "-workspace", ws)
	}
	if proj := strings.TrimSpace(payload["project"]); proj != "" {
		args = append(args, "-project", proj)
	}
	args = append(args, "-scheme", scheme)
	if cfg := strings.TrimSpace(payload["configuration"]); cfg != "" {
		args = append(args, "-configuration", cfg)
	}
	if sdk := strings.TrimSpace(payload["sdk"]); sdk != "" {
		args = append(args, "-sdk", sdk)
	}
	if dest := strings.TrimSpace(payload["destination"]); dest != "" {
		args = append(args, "-destination", dest)
	}
	if ddp := strings.TrimSpace(payload["derived_data_path"]); ddp != "" {
		args = append(args, "-derivedDataPath", ddp)
	}

	if signing.signed && signing.style == codeSignStyleAutomatic {
		args = append(args, "-allowProvisioningUpdates")
	}

	action := strings.TrimSpace(payload["action"])
	if action == "" {
		action = "build"
	}
	args = append(args, splitTasks(action)...)

	args = append(args, signing.signingBuildSettings()...)

	return args, nil
}

// buildIOSArchiveArgs constructs the xcodebuild argument vector for the archive
// phase of a signed export. It mirrors buildIOSArgs but forces the "archive"
// action and writes to -archivePath. Signing is required: an archive that will
// be exported to a distributable .ipa must be signed.
//
// Additional payload field:
//   - archive_path (required): -archivePath value (a .xcarchive bundle path)
func buildIOSArchiveArgs(payload map[string]string) ([]string, error) {
	scheme, err := nonEmptyField(payload, "scheme")
	if err != nil {
		return nil, err
	}
	archivePath, err := nonEmptyField(payload, "archive_path")
	if err != nil {
		return nil, err
	}
	signing, err := parseIOSSigning(payload)
	if err != nil {
		return nil, err
	}
	if !signing.signed {
		return nil, fmt.Errorf("archive/export requires signing parameters (team_id, signing_identity, or provisioning_profile)")
	}

	var args []string
	if ws := strings.TrimSpace(payload["workspace_file"]); ws != "" {
		args = append(args, "-workspace", ws)
	}
	if proj := strings.TrimSpace(payload["project"]); proj != "" {
		args = append(args, "-project", proj)
	}
	args = append(args, "-scheme", scheme)
	if cfg := strings.TrimSpace(payload["configuration"]); cfg != "" {
		args = append(args, "-configuration", cfg)
	}
	if dest := strings.TrimSpace(payload["destination"]); dest != "" {
		args = append(args, "-destination", dest)
	}
	if ddp := strings.TrimSpace(payload["derived_data_path"]); ddp != "" {
		args = append(args, "-derivedDataPath", ddp)
	}
	if signing.style == codeSignStyleAutomatic {
		args = append(args, "-allowProvisioningUpdates")
	}
	args = append(args, "archive", "-archivePath", archivePath)
	args = append(args, signing.signingBuildSettings()...)

	return args, nil
}

// buildIOSExportArgs constructs the `xcodebuild -exportArchive` argument vector.
//
// Additional payload fields:
//   - archive_path (required): the -archivePath produced by the archive phase
//   - export_path (required): -exportPath directory for the produced .ipa
//
// exportOptionsPlist is written by the handler to a temp file; its path is
// supplied here as the -exportOptionsPlist value.
func buildIOSExportArgs(payload map[string]string, exportOptionsPlistPath string) ([]string, error) {
	archivePath, err := nonEmptyField(payload, "archive_path")
	if err != nil {
		return nil, err
	}
	exportPath, err := nonEmptyField(payload, "export_path")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(exportOptionsPlistPath) == "" {
		return nil, fmt.Errorf("export requires an ExportOptions.plist path")
	}
	return []string{
		"-exportArchive",
		"-archivePath", archivePath,
		"-exportPath", exportPath,
		"-exportOptionsPlist", exportOptionsPlistPath,
	}, nil
}

// validExportMethods are the supported -exportOptionsPlist "method" values.
var validExportMethods = map[string]bool{
	"app-store":   true,
	"ad-hoc":      true,
	"enterprise":  true,
	"development": true,
	"validation":  true,
	"package":     true,
}

// buildExportOptionsPlist generates the XML content for an ExportOptions.plist
// used by `xcodebuild -exportArchive`.
//
// Payload fields consumed:
//   - export_method (optional): default "development". One of app-store, ad-hoc,
//     enterprise, development, validation, package.
//   - team_id (optional): teamID key
//   - provisioning_profile (optional): the bundle-id -> profile mapping requires
//     a bundle id; when provisioning_profile_bundle_id is supplied a
//     provisioningProfiles dict is emitted.
//   - provisioning_profile_bundle_id (optional): bundle id keyed in
//     provisioningProfiles.
//   - export_signing_style (optional): signingStyle, default derived from
//     code_sign_style ("manual"/"automatic").
func buildExportOptionsPlist(payload map[string]string) (string, error) {
	method := strings.TrimSpace(payload["export_method"])
	if method == "" {
		method = "development"
	}
	if !validExportMethods[method] {
		return "", fmt.Errorf("invalid export_method %q", method)
	}

	signing, err := parseIOSSigning(payload)
	if err != nil {
		return "", err
	}
	signingStyle := strings.ToLower(strings.TrimSpace(payload["export_signing_style"]))
	if signingStyle == "" {
		if signing.style != "" {
			signingStyle = signing.style
		} else {
			signingStyle = codeSignStyleManual
		}
	}
	if signingStyle != codeSignStyleManual && signingStyle != codeSignStyleAutomatic {
		return "", fmt.Errorf("invalid export_signing_style %q: must be %q or %q", signingStyle, codeSignStyleManual, codeSignStyleAutomatic)
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")
	writePlistString(&b, "method", method)
	writePlistString(&b, "signingStyle", signingStyle)
	if signing.teamID != "" {
		writePlistString(&b, "teamID", signing.teamID)
	}
	bundleID := strings.TrimSpace(payload["provisioning_profile_bundle_id"])
	if signing.provisioningProfile != "" && bundleID != "" {
		b.WriteString("  <key>provisioningProfiles</key>\n")
		b.WriteString("  <dict>\n")
		b.WriteString("    <key>" + plistEscape(bundleID) + "</key>\n")
		b.WriteString("    <string>" + plistEscape(signing.provisioningProfile) + "</string>\n")
		b.WriteString("  </dict>\n")
	}
	b.WriteString("</dict>\n")
	b.WriteString("</plist>\n")
	return b.String(), nil
}

func writePlistString(b *strings.Builder, key, value string) {
	b.WriteString("  <key>" + plistEscape(key) + "</key>\n")
	b.WriteString("  <string>" + plistEscape(value) + "</string>\n")
}

// plistEscape escapes the XML metacharacters that can appear in plist string
// values so a profile name or team id can never break the document.
func plistEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return r.Replace(s)
}

func (h *IOSBuildHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("IOS_BUILD requires macOS (xcodebuild); node is %s", runtime.GOOS)
	}

	// Archive+export path: requested when export_path is present. Produces a
	// signed .ipa via `xcodebuild archive` then `xcodebuild -exportArchive`.
	if strings.TrimSpace(job.Payload["export_path"]) != "" {
		return h.executeArchiveExport(ctx, job)
	}

	args, err := buildIOSArgs(job.Payload)
	if err != nil {
		return nil, err
	}

	cmdStr := "xcodebuild " + strings.Join(args, " ")
	ctx.Log("info", "     - [Job %s] IOS_BUILD: %s", job.ID, cmdStr)

	out, runErr := runBuildCommand(h.WorkspaceDir, "xcodebuild", args)
	if runErr != nil {
		return out, fmt.Errorf("xcodebuild failed: %w", runErr)
	}

	return marshalBuildResult(buildResult{
		Tool:         "xcodebuild",
		Command:      cmdStr,
		Output:       string(out),
		ArtifactPath: strings.TrimSpace(job.Payload["artifact_path"]),
	})
}

// executeArchiveExport runs the two-phase signed export: archive the scheme,
// generate an ExportOptions.plist, then export to a signed .ipa. The plist is
// written to a temp file (cleaned up on return).
func (h *IOSBuildHandler) executeArchiveExport(ctx JobContext, job *nexus.Job) ([]byte, error) {
	archiveArgs, err := buildIOSArchiveArgs(job.Payload)
	if err != nil {
		return nil, err
	}

	archiveCmd := "xcodebuild " + strings.Join(archiveArgs, " ")
	ctx.Log("info", "     - [Job %s] IOS_BUILD archive: %s", job.ID, archiveCmd)
	archiveOut, runErr := runBuildCommand(h.WorkspaceDir, "xcodebuild", archiveArgs)
	if runErr != nil {
		return archiveOut, fmt.Errorf("xcodebuild archive failed: %w", runErr)
	}

	plist, err := buildExportOptionsPlist(job.Payload)
	if err != nil {
		return nil, err
	}
	plistPath, cleanup, err := writeExportOptionsPlist(plist)
	if err != nil {
		return nil, fmt.Errorf("writing ExportOptions.plist: %w", err)
	}
	defer cleanup()

	exportArgs, err := buildIOSExportArgs(job.Payload, plistPath)
	if err != nil {
		return nil, err
	}
	exportCmd := "xcodebuild " + strings.Join(exportArgs, " ")
	ctx.Log("info", "     - [Job %s] IOS_BUILD export: %s", job.ID, exportCmd)
	exportOut, runErr := runBuildCommand(h.WorkspaceDir, "xcodebuild", exportArgs)
	if runErr != nil {
		return exportOut, fmt.Errorf("xcodebuild -exportArchive failed: %w", runErr)
	}

	return marshalBuildResult(buildResult{
		Tool:         "xcodebuild",
		Command:      archiveCmd + " && " + exportCmd,
		Output:       string(archiveOut) + string(exportOut),
		ArtifactPath: strings.TrimSpace(job.Payload["artifact_path"]),
	})
}

// writeExportOptionsPlist writes content to a temp file and returns its path and
// a cleanup func. It is a variable so tests can stub file IO if needed.
var writeExportOptionsPlist = func(content string) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "ExportOptions-*.plist")
	if err != nil {
		return "", func() {}, err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", func() {}, err
	}
	name := f.Name()
	return name, func() { os.Remove(name) }, nil
}

var _ JobHandler = (*IOSBuildHandler)(nil)

// AndroidBuildHandler handles ANDROID_BUILD jobs by invoking the project's
// Gradle wrapper. It runs on any platform but requires an Android SDK; the
// caller is expected to route only to SDK-equipped nodes, and Execute fails
// cleanly with a structured error if the wrapper is absent.
type AndroidBuildHandler struct {
	WorkspaceDir string
}

// NewAndroidBuildHandler creates an AndroidBuildHandler rooted at workspace.
func NewAndroidBuildHandler(workspace string) *AndroidBuildHandler {
	return &AndroidBuildHandler{WorkspaceDir: workspace}
}

// buildAndroidArgs constructs the gradlew argument vector from the payload.
//
// Payload fields:
//   - tasks (required): whitespace-separated gradle tasks, e.g. "assembleDebug"
//   - gradle_args (optional): extra flags, e.g. "--stacktrace -PflavorX"
func buildAndroidArgs(payload map[string]string) ([]string, error) {
	tasksRaw, err := nonEmptyField(payload, "tasks")
	if err != nil {
		return nil, err
	}
	tasks := splitTasks(tasksRaw)
	if len(tasks) == 0 {
		return nil, fmt.Errorf("job payload field \"tasks\" contains no tasks")
	}

	args := tasks
	if extra := strings.TrimSpace(payload["gradle_args"]); extra != "" {
		args = append(args, splitTasks(extra)...)
	}
	return args, nil
}

func (h *AndroidBuildHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	args, err := buildAndroidArgs(job.Payload)
	if err != nil {
		return nil, err
	}

	cmdStr := "./gradlew " + strings.Join(args, " ")
	ctx.Log("info", "     - [Job %s] ANDROID_BUILD: %s", job.ID, cmdStr)

	out, runErr := runBuildCommand(h.WorkspaceDir, "./gradlew", args)
	if runErr != nil {
		return out, fmt.Errorf("gradlew failed: %w", runErr)
	}

	return marshalBuildResult(buildResult{
		Tool:         "gradlew",
		Command:      cmdStr,
		Output:       string(out),
		ArtifactPath: strings.TrimSpace(job.Payload["artifact_path"]),
	})
}

var _ JobHandler = (*AndroidBuildHandler)(nil)

// GomobileBuildHandler handles GOMOBILE_BUILD jobs by cross-compiling a Go
// package to a mobile framework via `gomobile bind`. The iOS (.xcframework)
// target is gated to darwin at runtime; the Android (.aar) target runs on any
// platform with the Android SDK + NDK.
type GomobileBuildHandler struct {
	WorkspaceDir string
}

// NewGomobileBuildHandler creates a GomobileBuildHandler rooted at workspace.
func NewGomobileBuildHandler(workspace string) *GomobileBuildHandler {
	return &GomobileBuildHandler{WorkspaceDir: workspace}
}

// gomobileTarget is a validated gomobile -target value.
const (
	gomobileTargetIOS     = "ios"
	gomobileTargetAndroid = "android"
)

// buildGomobileArgs constructs the `gomobile bind` argument vector.
//
// Payload fields:
//   - target (required): "ios" or "android"
//   - package (required): the Go import path to bind, e.g. "./mobile"
//   - output (optional): -o output path (.xcframework or .aar)
//   - bind_args (optional): extra flags, e.g. "-ldflags=-s"
//
// goosForTarget is the host GOOS required for the requested target; iOS binds
// can only be produced on darwin. The returned GOOS lets Execute gate at
// runtime; empty means "any host".
func buildGomobileArgs(payload map[string]string) (args []string, requiredGOOS string, err error) {
	target, err := nonEmptyField(payload, "target")
	if err != nil {
		return nil, "", err
	}
	target = strings.ToLower(target)
	switch target {
	case gomobileTargetIOS:
		requiredGOOS = "darwin"
	case gomobileTargetAndroid:
		requiredGOOS = ""
	default:
		return nil, "", fmt.Errorf("invalid target %q: must be %q or %q", target, gomobileTargetIOS, gomobileTargetAndroid)
	}

	pkg, err := nonEmptyField(payload, "package")
	if err != nil {
		return nil, "", err
	}

	args = []string{"bind", "-target", target}
	if out := strings.TrimSpace(payload["output"]); out != "" {
		args = append(args, "-o", out)
	}
	if extra := strings.TrimSpace(payload["bind_args"]); extra != "" {
		args = append(args, splitTasks(extra)...)
	}
	args = append(args, pkg)
	return args, requiredGOOS, nil
}

func (h *GomobileBuildHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	args, requiredGOOS, err := buildGomobileArgs(job.Payload)
	if err != nil {
		return nil, err
	}
	if requiredGOOS != "" && runtime.GOOS != requiredGOOS {
		return nil, fmt.Errorf("GOMOBILE_BUILD target %q requires %s; node is %s",
			job.Payload["target"], requiredGOOS, runtime.GOOS)
	}

	cmdStr := "gomobile " + strings.Join(args, " ")
	ctx.Log("info", "     - [Job %s] GOMOBILE_BUILD: %s", job.ID, cmdStr)

	out, runErr := runBuildCommand(h.WorkspaceDir, "gomobile", args)
	if runErr != nil {
		return out, fmt.Errorf("gomobile bind failed: %w", runErr)
	}

	return marshalBuildResult(buildResult{
		Tool:         "gomobile",
		Command:      cmdStr,
		Output:       string(out),
		ArtifactPath: strings.TrimSpace(job.Payload["output"]),
	})
}

var _ JobHandler = (*GomobileBuildHandler)(nil)

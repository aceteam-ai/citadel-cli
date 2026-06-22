// internal/jobs/build_handlers.go
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
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
func buildIOSArgs(payload map[string]string) ([]string, error) {
	scheme, err := nonEmptyField(payload, "scheme")
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

	action := strings.TrimSpace(payload["action"])
	if action == "" {
		action = "build"
	}
	args = append(args, splitTasks(action)...)

	return args, nil
}

func (h *IOSBuildHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("IOS_BUILD requires macOS (xcodebuild); node is %s", runtime.GOOS)
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

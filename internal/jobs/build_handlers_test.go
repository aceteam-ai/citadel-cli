package jobs

import (
	"encoding/json"
	"errors"
	"reflect"
	"runtime"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// stubBuildCommand replaces runBuildCommand for the duration of a test, capturing
// the invocation and returning the supplied output/error. It restores the
// original on cleanup so tests stay isolated.
func stubBuildCommand(t *testing.T, out []byte, err error) *struct {
	called    bool
	workspace string
	name      string
	args      []string
} {
	t.Helper()
	rec := &struct {
		called    bool
		workspace string
		name      string
		args      []string
	}{}
	orig := runBuildCommand
	runBuildCommand = func(workspace, name string, args []string) ([]byte, error) {
		rec.called = true
		rec.workspace = workspace
		rec.name = name
		rec.args = args
		return out, err
	}
	t.Cleanup(func() { runBuildCommand = orig })
	return rec
}

func TestBuildIOSArgs(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]string
		want    []string
		wantErr bool
	}{
		{
			name:    "missing scheme",
			payload: map[string]string{},
			wantErr: true,
		},
		{
			name:    "blank scheme",
			payload: map[string]string{"scheme": "   "},
			wantErr: true,
		},
		{
			name:    "scheme only defaults to build action",
			payload: map[string]string{"scheme": "MyApp"},
			want:    []string{"-scheme", "MyApp", "build"},
		},
		{
			name: "full configuration",
			payload: map[string]string{
				"workspace_file": "MyApp.xcworkspace",
				"scheme":         "MyApp",
				"configuration":  "Release",
				"sdk":            "iphonesimulator",
				"destination":    "generic/platform=iOS",
			},
			want: []string{
				"-workspace", "MyApp.xcworkspace",
				"-scheme", "MyApp",
				"-configuration", "Release",
				"-sdk", "iphonesimulator",
				"-destination", "generic/platform=iOS",
				"build",
			},
		},
		{
			name: "project and explicit clean+build action",
			payload: map[string]string{
				"project": "MyApp.xcodeproj",
				"scheme":  "MyApp",
				"action":  "clean build",
			},
			want: []string{
				"-project", "MyApp.xcodeproj",
				"-scheme", "MyApp",
				"clean", "build",
			},
		},
		{
			name: "derived data path",
			payload: map[string]string{
				"scheme":            "MyApp",
				"derived_data_path": "/tmp/dd",
			},
			want: []string{"-scheme", "MyApp", "-derivedDataPath", "/tmp/dd", "build"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildIOSArgs(tc.payload)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got args %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("args = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildAndroidArgs(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]string
		want    []string
		wantErr bool
	}{
		{
			name:    "missing tasks",
			payload: map[string]string{},
			wantErr: true,
		},
		{
			name:    "blank tasks",
			payload: map[string]string{"tasks": "   "},
			wantErr: true,
		},
		{
			name:    "single task",
			payload: map[string]string{"tasks": "assembleDebug"},
			want:    []string{"assembleDebug"},
		},
		{
			name:    "multiple tasks plus extra args",
			payload: map[string]string{"tasks": "clean assembleRelease", "gradle_args": "--stacktrace -Pflavor=pro"},
			want:    []string{"clean", "assembleRelease", "--stacktrace", "-Pflavor=pro"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildAndroidArgs(tc.payload)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got args %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("args = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildGomobileArgs(t *testing.T) {
	tests := []struct {
		name     string
		payload  map[string]string
		wantArgs []string
		wantGOOS string
		wantErr  bool
	}{
		{
			name:    "missing target",
			payload: map[string]string{"package": "./mobile"},
			wantErr: true,
		},
		{
			name:    "invalid target",
			payload: map[string]string{"target": "web", "package": "./mobile"},
			wantErr: true,
		},
		{
			name:    "missing package",
			payload: map[string]string{"target": "ios"},
			wantErr: true,
		},
		{
			name:     "ios requires darwin",
			payload:  map[string]string{"target": "ios", "package": "./mobile", "output": "Mobile.xcframework"},
			wantArgs: []string{"bind", "-target", "ios", "-o", "Mobile.xcframework", "./mobile"},
			wantGOOS: "darwin",
		},
		{
			name:     "android any host",
			payload:  map[string]string{"target": "ANDROID", "package": "./mobile", "output": "mobile.aar", "bind_args": "-ldflags=-s"},
			wantArgs: []string{"bind", "-target", "android", "-o", "mobile.aar", "-ldflags=-s", "./mobile"},
			wantGOOS: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args, goos, err := buildGomobileArgs(tc.payload)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got args %v", args)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(args, tc.wantArgs) {
				t.Errorf("args = %v, want %v", args, tc.wantArgs)
			}
			if goos != tc.wantGOOS {
				t.Errorf("requiredGOOS = %q, want %q", goos, tc.wantGOOS)
			}
		})
	}
}

func jobWith(jobType string, payload map[string]string) *nexus.Job {
	return &nexus.Job{ID: "test", Type: jobType, Payload: payload}
}

func TestIOSBuildHandler_PlatformGate(t *testing.T) {
	// Stub the exec layer so darwin nodes don't invoke a real xcodebuild; we only
	// care whether the platform gate short-circuits before the command runs.
	rec := stubBuildCommand(t, []byte("** BUILD SUCCEEDED **"), nil)
	h := NewIOSBuildHandler("")
	_, err := h.Execute(JobContext{}, jobWith("IOS_BUILD", map[string]string{"scheme": "MyApp"}))
	if runtime.GOOS == "darwin" {
		if err != nil {
			t.Fatalf("darwin node should pass the platform gate: %v", err)
		}
		if !rec.called {
			t.Fatal("darwin node should reach the build command")
		}
	} else {
		if err == nil {
			t.Fatal("non-darwin node should reject IOS_BUILD")
		}
		if rec.called {
			t.Fatal("runBuildCommand must not run when platform gate fails")
		}
	}
}

func TestIOSBuildHandler_Success(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("IOS_BUILD only executes on darwin")
	}
	rec := stubBuildCommand(t, []byte("** BUILD SUCCEEDED **"), nil)
	h := NewIOSBuildHandler("/work")
	out, err := h.Execute(JobContext{}, jobWith("IOS_BUILD", map[string]string{
		"scheme":        "MyApp",
		"configuration": "Release",
		"artifact_path": "/work/build/MyApp.app",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rec.called {
		t.Fatal("expected runBuildCommand to be called")
	}
	if rec.name != "xcodebuild" {
		t.Errorf("command = %q, want xcodebuild", rec.name)
	}
	if rec.workspace != "/work" {
		t.Errorf("workspace = %q, want /work", rec.workspace)
	}
	var res buildResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if res.Tool != "xcodebuild" {
		t.Errorf("tool = %q, want xcodebuild", res.Tool)
	}
	if res.ArtifactPath != "/work/build/MyApp.app" {
		t.Errorf("artifact_path = %q", res.ArtifactPath)
	}
	if res.Output != "** BUILD SUCCEEDED **" {
		t.Errorf("output = %q", res.Output)
	}
}

func TestIOSBuildHandler_CommandFailurePropagates(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("IOS_BUILD only executes on darwin")
	}
	stubBuildCommand(t, []byte("** BUILD FAILED **"), errors.New("exit status 65"))
	h := NewIOSBuildHandler("")
	out, err := h.Execute(JobContext{}, jobWith("IOS_BUILD", map[string]string{"scheme": "MyApp"}))
	if err == nil {
		t.Fatal("expected build failure error")
	}
	// On failure the handler returns the raw combined output (not JSON) so the
	// caller surfaces the build log.
	if string(out) != "** BUILD FAILED **" {
		t.Errorf("output = %q, want raw build log", string(out))
	}
}

func TestIOSBuildHandler_InvalidInput(t *testing.T) {
	h := NewIOSBuildHandler("")
	if runtime.GOOS != "darwin" {
		// Non-darwin hits the platform gate before validation; nothing to assert here.
		t.Skip("validation path only reachable on darwin")
	}
	_, err := h.Execute(JobContext{}, jobWith("IOS_BUILD", map[string]string{}))
	if err == nil {
		t.Fatal("expected error for missing scheme")
	}
}

func TestAndroidBuildHandler_Success(t *testing.T) {
	rec := stubBuildCommand(t, []byte("BUILD SUCCESSFUL"), nil)
	h := NewAndroidBuildHandler("/proj")
	out, err := h.Execute(JobContext{}, jobWith("ANDROID_BUILD", map[string]string{
		"tasks":         "assembleDebug",
		"artifact_path": "/proj/app/build/outputs/apk/debug/app-debug.apk",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.name != "./gradlew" {
		t.Errorf("command = %q, want ./gradlew", rec.name)
	}
	if !reflect.DeepEqual(rec.args, []string{"assembleDebug"}) {
		t.Errorf("args = %v", rec.args)
	}
	var res buildResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if res.Tool != "gradlew" {
		t.Errorf("tool = %q", res.Tool)
	}
}

func TestAndroidBuildHandler_MissingTasks(t *testing.T) {
	rec := stubBuildCommand(t, nil, nil)
	h := NewAndroidBuildHandler("")
	_, err := h.Execute(JobContext{}, jobWith("ANDROID_BUILD", map[string]string{}))
	if err == nil {
		t.Fatal("expected error for missing tasks")
	}
	if rec.called {
		t.Fatal("runBuildCommand must not run when validation fails")
	}
}

func TestAndroidBuildHandler_FailurePropagates(t *testing.T) {
	stubBuildCommand(t, []byte("BUILD FAILED"), errors.New("exit status 1"))
	h := NewAndroidBuildHandler("")
	out, err := h.Execute(JobContext{}, jobWith("ANDROID_BUILD", map[string]string{"tasks": "assembleDebug"}))
	if err == nil {
		t.Fatal("expected error")
	}
	if string(out) != "BUILD FAILED" {
		t.Errorf("output = %q", string(out))
	}
}

func TestGomobileBuildHandler_AndroidSuccess(t *testing.T) {
	rec := stubBuildCommand(t, []byte(""), nil)
	h := NewGomobileBuildHandler("/src")
	out, err := h.Execute(JobContext{}, jobWith("GOMOBILE_BUILD", map[string]string{
		"target":  "android",
		"package": "./mobile",
		"output":  "mobile.aar",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.name != "gomobile" {
		t.Errorf("command = %q, want gomobile", rec.name)
	}
	var res buildResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if res.ArtifactPath != "mobile.aar" {
		t.Errorf("artifact_path = %q", res.ArtifactPath)
	}
}

func TestGomobileBuildHandler_IOSPlatformGate(t *testing.T) {
	rec := stubBuildCommand(t, nil, nil)
	h := NewGomobileBuildHandler("")
	_, err := h.Execute(JobContext{}, jobWith("GOMOBILE_BUILD", map[string]string{
		"target":  "ios",
		"package": "./mobile",
	}))
	if runtime.GOOS == "darwin" {
		if err != nil {
			t.Fatalf("darwin should pass the gomobile ios gate: %v", err)
		}
	} else {
		if err == nil {
			t.Fatal("non-darwin should reject gomobile ios target")
		}
		if rec.called {
			t.Fatal("runBuildCommand must not run when platform gate fails")
		}
	}
}

func TestGomobileBuildHandler_InvalidTarget(t *testing.T) {
	rec := stubBuildCommand(t, nil, nil)
	h := NewGomobileBuildHandler("")
	_, err := h.Execute(JobContext{}, jobWith("GOMOBILE_BUILD", map[string]string{
		"target":  "wasm",
		"package": "./mobile",
	}))
	if err == nil {
		t.Fatal("expected error for invalid target")
	}
	if rec.called {
		t.Fatal("runBuildCommand must not run for invalid target")
	}
}

package jobs

import (
	"encoding/json"
	"errors"
	"reflect"
	"runtime"
	"strings"
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
			name:    "scheme only defaults to build action (unsigned)",
			payload: map[string]string{"scheme": "MyApp"},
			want:    []string{"-scheme", "MyApp", "build", "CODE_SIGNING_ALLOWED=NO"},
		},
		{
			name: "full configuration (unsigned)",
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
				"CODE_SIGNING_ALLOWED=NO",
			},
		},
		{
			name: "project and explicit clean+build action (unsigned)",
			payload: map[string]string{
				"project": "MyApp.xcodeproj",
				"scheme":  "MyApp",
				"action":  "clean build",
			},
			want: []string{
				"-project", "MyApp.xcodeproj",
				"-scheme", "MyApp",
				"clean", "build",
				"CODE_SIGNING_ALLOWED=NO",
			},
		},
		{
			name: "derived data path (unsigned)",
			payload: map[string]string{
				"scheme":            "MyApp",
				"derived_data_path": "/tmp/dd",
			},
			want: []string{"-scheme", "MyApp", "-derivedDataPath", "/tmp/dd", "build", "CODE_SIGNING_ALLOWED=NO"},
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

func TestBuildIOSArgs_Signing(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]string
		want    []string
		wantErr bool
	}{
		{
			name:    "unsigned build appends CODE_SIGNING_ALLOWED=NO",
			payload: map[string]string{"scheme": "MyApp", "sdk": "iphonesimulator"},
			want: []string{
				"-scheme", "MyApp",
				"-sdk", "iphonesimulator",
				"build",
				"CODE_SIGNING_ALLOWED=NO",
			},
		},
		{
			name: "manual signing with all params",
			payload: map[string]string{
				"scheme":               "MyApp",
				"configuration":        "Release",
				"team_id":              "ABCDE12345",
				"signing_identity":     "Apple Distribution",
				"provisioning_profile": "MyApp Distribution",
				"code_sign_style":      "manual",
			},
			want: []string{
				"-scheme", "MyApp",
				"-configuration", "Release",
				"build",
				"CODE_SIGN_STYLE=Manual",
				"DEVELOPMENT_TEAM=ABCDE12345",
				"CODE_SIGN_IDENTITY=Apple Distribution",
				"PROVISIONING_PROFILE_SPECIFIER=MyApp Distribution",
			},
		},
		{
			name: "team_id only defaults to manual style",
			payload: map[string]string{
				"scheme":  "MyApp",
				"team_id": "ABCDE12345",
			},
			want: []string{
				"-scheme", "MyApp",
				"build",
				"CODE_SIGN_STYLE=Manual",
				"DEVELOPMENT_TEAM=ABCDE12345",
			},
		},
		{
			name: "automatic style adds -allowProvisioningUpdates",
			payload: map[string]string{
				"scheme":          "MyApp",
				"team_id":         "ABCDE12345",
				"code_sign_style": "automatic",
			},
			want: []string{
				"-scheme", "MyApp",
				"-allowProvisioningUpdates",
				"build",
				"CODE_SIGN_STYLE=Automatic",
				"DEVELOPMENT_TEAM=ABCDE12345",
			},
		},
		{
			name: "invalid code_sign_style rejected",
			payload: map[string]string{
				"scheme":          "MyApp",
				"team_id":         "ABCDE12345",
				"code_sign_style": "adhoc",
			},
			wantErr: true,
		},
		{
			name: "manual signing without team_id rejected",
			payload: map[string]string{
				"scheme":           "MyApp",
				"signing_identity": "Apple Distribution",
			},
			wantErr: true,
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

func TestBuildIOSArchiveArgs(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]string
		want    []string
		wantErr bool
	}{
		{
			name:    "missing scheme",
			payload: map[string]string{"archive_path": "/tmp/MyApp.xcarchive", "team_id": "ABCDE12345"},
			wantErr: true,
		},
		{
			name:    "missing archive_path",
			payload: map[string]string{"scheme": "MyApp", "team_id": "ABCDE12345"},
			wantErr: true,
		},
		{
			name:    "archive requires signing params",
			payload: map[string]string{"scheme": "MyApp", "archive_path": "/tmp/MyApp.xcarchive"},
			wantErr: true,
		},
		{
			name: "manual archive",
			payload: map[string]string{
				"workspace_file":       "MyApp.xcworkspace",
				"scheme":               "MyApp",
				"configuration":        "Release",
				"archive_path":         "/tmp/MyApp.xcarchive",
				"team_id":              "ABCDE12345",
				"signing_identity":     "Apple Distribution",
				"provisioning_profile": "MyApp Distribution",
			},
			want: []string{
				"-workspace", "MyApp.xcworkspace",
				"-scheme", "MyApp",
				"-configuration", "Release",
				"archive", "-archivePath", "/tmp/MyApp.xcarchive",
				"CODE_SIGN_STYLE=Manual",
				"DEVELOPMENT_TEAM=ABCDE12345",
				"CODE_SIGN_IDENTITY=Apple Distribution",
				"PROVISIONING_PROFILE_SPECIFIER=MyApp Distribution",
			},
		},
		{
			name: "automatic archive adds -allowProvisioningUpdates",
			payload: map[string]string{
				"scheme":          "MyApp",
				"archive_path":    "/tmp/MyApp.xcarchive",
				"team_id":         "ABCDE12345",
				"code_sign_style": "automatic",
			},
			want: []string{
				"-scheme", "MyApp",
				"-allowProvisioningUpdates",
				"archive", "-archivePath", "/tmp/MyApp.xcarchive",
				"CODE_SIGN_STYLE=Automatic",
				"DEVELOPMENT_TEAM=ABCDE12345",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildIOSArchiveArgs(tc.payload)
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

func TestBuildIOSExportArgs(t *testing.T) {
	tests := []struct {
		name      string
		payload   map[string]string
		plistPath string
		want      []string
		wantErr   bool
	}{
		{
			name:      "missing archive_path",
			payload:   map[string]string{"export_path": "/tmp/out"},
			plistPath: "/tmp/ExportOptions.plist",
			wantErr:   true,
		},
		{
			name:      "missing export_path",
			payload:   map[string]string{"archive_path": "/tmp/MyApp.xcarchive"},
			plistPath: "/tmp/ExportOptions.plist",
			wantErr:   true,
		},
		{
			name:      "missing plist path",
			payload:   map[string]string{"archive_path": "/tmp/MyApp.xcarchive", "export_path": "/tmp/out"},
			plistPath: "",
			wantErr:   true,
		},
		{
			name:      "valid export",
			payload:   map[string]string{"archive_path": "/tmp/MyApp.xcarchive", "export_path": "/tmp/out"},
			plistPath: "/tmp/ExportOptions.plist",
			want: []string{
				"-exportArchive",
				"-archivePath", "/tmp/MyApp.xcarchive",
				"-exportPath", "/tmp/out",
				"-exportOptionsPlist", "/tmp/ExportOptions.plist",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildIOSExportArgs(tc.payload, tc.plistPath)
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

func TestBuildExportOptionsPlist(t *testing.T) {
	t.Run("invalid export_method rejected", func(t *testing.T) {
		_, err := buildExportOptionsPlist(map[string]string{"export_method": "carrier", "team_id": "ABCDE12345"})
		if err == nil {
			t.Fatal("expected error for invalid export_method")
		}
	})

	t.Run("default method development with manual style", func(t *testing.T) {
		got, err := buildExportOptionsPlist(map[string]string{"team_id": "ABCDE12345"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, want := range []string{
			"<key>method</key>",
			"<string>development</string>",
			"<key>signingStyle</key>",
			"<string>manual</string>",
			"<key>teamID</key>",
			"<string>ABCDE12345</string>",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("plist missing %q\n%s", want, got)
			}
		}
		if strings.Contains(got, "provisioningProfiles") {
			t.Errorf("did not expect provisioningProfiles without bundle id\n%s", got)
		}
	})

	t.Run("app-store with provisioning profile mapping", func(t *testing.T) {
		got, err := buildExportOptionsPlist(map[string]string{
			"export_method":                  "app-store",
			"team_id":                        "ABCDE12345",
			"provisioning_profile":           "MyApp AppStore",
			"provisioning_profile_bundle_id": "com.example.MyApp",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, want := range []string{
			"<string>app-store</string>",
			"<key>provisioningProfiles</key>",
			"<key>com.example.MyApp</key>",
			"<string>MyApp AppStore</string>",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("plist missing %q\n%s", want, got)
			}
		}
	})

	t.Run("escapes XML metacharacters", func(t *testing.T) {
		got, err := buildExportOptionsPlist(map[string]string{
			"team_id":                        "ABCDE12345",
			"provisioning_profile":           "Profile <A&B>",
			"provisioning_profile_bundle_id": "com.example.app",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(got, "Profile &lt;A&amp;B&gt;") {
			t.Errorf("expected escaped profile name\n%s", got)
		}
	})

	t.Run("explicit export_signing_style overrides", func(t *testing.T) {
		got, err := buildExportOptionsPlist(map[string]string{
			"team_id":              "ABCDE12345",
			"code_sign_style":      "manual",
			"export_signing_style": "automatic",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(got, "<string>automatic</string>") {
			t.Errorf("expected automatic signingStyle\n%s", got)
		}
	})

	t.Run("invalid export_signing_style rejected", func(t *testing.T) {
		_, err := buildExportOptionsPlist(map[string]string{
			"team_id":              "ABCDE12345",
			"export_signing_style": "bogus",
		})
		if err == nil {
			t.Fatal("expected error for invalid export_signing_style")
		}
	})
}

func TestIOSBuildHandler_ArchiveExportFlow(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("IOS_BUILD only executes on darwin")
	}

	// Capture every xcodebuild invocation so we can assert the archive phase runs
	// before the export phase.
	var calls [][]string
	orig := runBuildCommand
	runBuildCommand = func(workspace, name string, args []string) ([]byte, error) {
		calls = append(calls, args)
		return []byte("** OK **"), nil
	}
	t.Cleanup(func() { runBuildCommand = orig })

	// Stub the plist writer so no real temp file is needed and we can confirm the
	// generated content reaches the export args.
	origWriter := writeExportOptionsPlist
	var wroteContent string
	writeExportOptionsPlist = func(content string) (string, func(), error) {
		wroteContent = content
		return "/tmp/stub-ExportOptions.plist", func() {}, nil
	}
	t.Cleanup(func() { writeExportOptionsPlist = origWriter })

	h := NewIOSBuildHandler("/work")
	out, err := h.Execute(JobContext{}, jobWith("IOS_BUILD", map[string]string{
		"scheme":               "MyApp",
		"configuration":        "Release",
		"archive_path":         "/tmp/MyApp.xcarchive",
		"export_path":          "/tmp/out",
		"export_method":        "app-store",
		"team_id":              "ABCDE12345",
		"signing_identity":     "Apple Distribution",
		"provisioning_profile": "MyApp AppStore",
		"artifact_path":        "/tmp/out/MyApp.ipa",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 xcodebuild calls (archive, export), got %d", len(calls))
	}
	if !containsArg(calls[0], "archive") {
		t.Errorf("first call should be the archive phase: %v", calls[0])
	}
	if !containsArg(calls[1], "-exportArchive") {
		t.Errorf("second call should be the export phase: %v", calls[1])
	}
	if !containsArg(calls[1], "-exportOptionsPlist") {
		t.Errorf("export phase should reference the plist: %v", calls[1])
	}
	if !strings.Contains(wroteContent, "<string>app-store</string>") {
		t.Errorf("plist content missing method: %s", wroteContent)
	}

	var res buildResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if res.ArtifactPath != "/tmp/out/MyApp.ipa" {
		t.Errorf("artifact_path = %q", res.ArtifactPath)
	}
}

func TestIOSBuildHandler_ArchiveFailureStops(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("IOS_BUILD only executes on darwin")
	}
	var calls int
	orig := runBuildCommand
	runBuildCommand = func(workspace, name string, args []string) ([]byte, error) {
		calls++
		return []byte("** ARCHIVE FAILED **"), errors.New("exit status 65")
	}
	t.Cleanup(func() { runBuildCommand = orig })

	h := NewIOSBuildHandler("/work")
	out, err := h.Execute(JobContext{}, jobWith("IOS_BUILD", map[string]string{
		"scheme":       "MyApp",
		"archive_path": "/tmp/MyApp.xcarchive",
		"export_path":  "/tmp/out",
		"team_id":      "ABCDE12345",
	}))
	if err == nil {
		t.Fatal("expected archive failure to propagate")
	}
	if calls != 1 {
		t.Errorf("export phase must not run after archive failure; calls=%d", calls)
	}
	if string(out) != "** ARCHIVE FAILED **" {
		t.Errorf("output = %q, want raw archive log", string(out))
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
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

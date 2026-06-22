package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestAndroidSDKFromEnv(t *testing.T) {
	tests := []struct {
		name        string
		androidHome string
		sdkRoot     string
		want        string
	}{
		{name: "ANDROID_HOME wins", androidHome: "/home", sdkRoot: "/root", want: "/home"},
		{name: "falls back to ANDROID_SDK_ROOT", androidHome: "", sdkRoot: "/root", want: "/root"},
		{name: "neither set", androidHome: "", sdkRoot: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ANDROID_HOME", tc.androidHome)
			t.Setenv("ANDROID_SDK_ROOT", tc.sdkRoot)
			if got := androidSDKFromEnv(); got != tc.want {
				t.Errorf("androidSDKFromEnv() = %q, want %q", got, tc.want)
			}
		})
	}
}

// resetIOSFlags restores the package-level iOS flag vars between subtests so
// state from one run does not leak into the next.
func resetIOSFlags() {
	provisionDryRun = false
	provisionIOSKeychain = "citadel-build.keychain-db"
	provisionIOSKeychainPass = ""
	provisionIOSCertPath = ""
	provisionIOSCertPass = ""
	provisionIOSProfiles = nil
	provisionIOSProfilesDir = ""
}

func TestRunProvisionIOS_DryRunPlan(t *testing.T) {
	resetIOSFlags()
	t.Cleanup(resetIOSFlags)

	provisionDryRun = true
	provisionIOSKeychainPass = "SECRETPW"
	provisionIOSProfilesDir = t.TempDir()

	var buf bytes.Buffer
	provisionIOSCmd.SetOut(&buf)
	t.Cleanup(func() { provisionIOSCmd.SetOut(nil) })

	if err := runProvisionIOS(provisionIOSCmd, nil); err != nil {
		t.Fatalf("dry-run iOS provisioning failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "security create-keychain") {
		t.Errorf("expected keychain creation in plan:\n%s", out)
	}
	if strings.Contains(out, "SECRETPW") {
		t.Errorf("keychain password leaked in dry-run output:\n%s", out)
	}
	if !strings.Contains(out, "<redacted>") {
		t.Errorf("password not redacted:\n%s", out)
	}
}

func TestRunProvisionIOS_RequiresKeychainPassword(t *testing.T) {
	resetIOSFlags()
	t.Cleanup(resetIOSFlags)

	provisionDryRun = true
	provisionIOSKeychainPass = "" // missing

	var buf bytes.Buffer
	provisionIOSCmd.SetOut(&buf)
	t.Cleanup(func() { provisionIOSCmd.SetOut(nil) })

	err := runProvisionIOS(provisionIOSCmd, nil)
	if err == nil {
		t.Fatal("expected error for missing keychain password")
	}
	if !strings.Contains(err.Error(), "keychain password is required") {
		t.Errorf("error = %q, want keychain-password message", err)
	}
}

func resetAndroidFlags() {
	provisionDryRun = false
	provisionAndroidSDKRoot = ""
	provisionAndroidLicenses = false
	provisionAndroidPackages = nil
}

func TestRunProvisionAndroid_DryRunPlan(t *testing.T) {
	resetAndroidFlags()
	t.Cleanup(resetAndroidFlags)

	provisionDryRun = true
	provisionAndroidSDKRoot = "/opt/android-sdk"
	provisionAndroidLicenses = true
	provisionAndroidPackages = []string{"platform-tools"}

	var buf bytes.Buffer
	provisionAndroidCmd.SetOut(&buf)
	t.Cleanup(func() { provisionAndroidCmd.SetOut(nil) })

	if err := runProvisionAndroid(provisionAndroidCmd, nil); err != nil {
		t.Fatalf("dry-run android provisioning failed: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "sdkmanager --sdk_root=/opt/android-sdk --licenses") {
		t.Errorf("expected licenses command:\n%s", out)
	}
	if !strings.Contains(out, "sdkmanager --sdk_root=/opt/android-sdk platform-tools") {
		t.Errorf("expected package install command:\n%s", out)
	}
}

func TestRunProvisionAndroid_NoSDKLocation(t *testing.T) {
	resetAndroidFlags()
	t.Cleanup(resetAndroidFlags)
	t.Setenv("ANDROID_HOME", "")
	t.Setenv("ANDROID_SDK_ROOT", "")

	provisionDryRun = true // dry-run so sdkmanager PATH check is skipped

	err := runProvisionAndroid(provisionAndroidCmd, nil)
	if err == nil {
		t.Fatal("expected error when no SDK location is available")
	}
	if !strings.Contains(err.Error(), "Android SDK location not found") {
		t.Errorf("error = %q, want SDK-location message", err)
	}
}

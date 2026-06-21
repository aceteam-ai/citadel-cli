package mobile

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeTemp creates a file with the given basename under a fresh temp dir and
// returns its path. Used so existence checks in the builders pass.
func writeTemp(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := writeFile(p); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}
	return p
}

func TestBuildIOSSteps_Validation(t *testing.T) {
	goodProfile := writeTemp(t, "app.mobileprovision")
	goodCert := writeTemp(t, "dist.p12")

	tests := []struct {
		name    string
		opts    IOSOptions
		wantErr string
	}{
		{
			name:    "missing keychain name",
			opts:    IOSOptions{KeychainPassword: "pw"},
			wantErr: "keychain name is required",
		},
		{
			name:    "missing keychain password",
			opts:    IOSOptions{KeychainName: "kc"},
			wantErr: "keychain password is required",
		},
		{
			name:    "cert path does not exist",
			opts:    IOSOptions{KeychainName: "kc", KeychainPassword: "pw", CertPath: "/nope/missing.p12", CertPassword: "x"},
			wantErr: "certificate not found",
		},
		{
			name:    "cert without password",
			opts:    IOSOptions{KeychainName: "kc", KeychainPassword: "pw", CertPath: goodCert},
			wantErr: "certificate password is required",
		},
		{
			name:    "profile missing on disk",
			opts:    IOSOptions{KeychainName: "kc", KeychainPassword: "pw", ProfilePaths: []string{"/nope/x.mobileprovision"}},
			wantErr: "provisioning profile not found",
		},
		{
			name:    "profile wrong extension",
			opts:    IOSOptions{KeychainName: "kc", KeychainPassword: "pw", ProfilePaths: []string{writeTemp(t, "wrong.txt")}},
			wantErr: ".mobileprovision extension",
		},
		{
			name: "valid minimal",
			opts: IOSOptions{KeychainName: "kc", KeychainPassword: "pw"},
		},
		{
			name: "valid full",
			opts: IOSOptions{
				KeychainName: "kc", KeychainPassword: "pw",
				CertPath: goodCert, CertPassword: "cp",
				ProfilePaths: []string{goodProfile}, ProfilesDir: "/tmp/pp",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildIOSSteps(tc.opts)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestBuildIOSSteps_PlanOrder(t *testing.T) {
	cert := writeTemp(t, "dist.p12")
	profile := writeTemp(t, "app.mobileprovision")

	steps, err := BuildIOSSteps(IOSOptions{
		KeychainName:     "citadel-build.keychain-db",
		KeychainPassword: "kcpw",
		CertPath:         cert,
		CertPassword:     "certpw",
		ProfilePaths:     []string{profile},
		ProfilesDir:      "/tmp/Provisioning Profiles",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expected: create, unlock, settings, import, partition-list, copy-profile.
	if len(steps) != 6 {
		t.Fatalf("got %d steps, want 6: %+v", len(steps), steps)
	}

	wantExec := []string{"create-keychain", "unlock-keychain", "set-keychain-settings", "import", "set-key-partition-list"}
	for i, want := range wantExec {
		if steps[i].Kind != StepExec {
			t.Errorf("step %d kind = %v, want StepExec", i, steps[i].Kind)
		}
		if steps[i].Name != securityBin {
			t.Errorf("step %d name = %q, want %q", i, steps[i].Name, securityBin)
		}
		if len(steps[i].Args) == 0 || steps[i].Args[0] != want {
			t.Errorf("step %d first arg = %v, want %q", i, steps[i].Args, want)
		}
	}

	last := steps[5]
	if last.Kind != StepCopyFile {
		t.Fatalf("last step kind = %v, want StepCopyFile", last.Kind)
	}
	if last.SrcPath != profile {
		t.Errorf("copy src = %q, want %q", last.SrcPath, profile)
	}
	wantDst := filepath.Join("/tmp/Provisioning Profiles", "app.mobileprovision")
	if last.DstPath != wantDst {
		t.Errorf("copy dst = %q, want %q", last.DstPath, wantDst)
	}
}

func TestBuildIOSSteps_NoCertNoProfile(t *testing.T) {
	steps, err := BuildIOSSteps(IOSOptions{KeychainName: "kc", KeychainPassword: "pw"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only create/unlock/settings — no import, partition-list, or copy.
	if len(steps) != 3 {
		t.Fatalf("got %d steps, want 3: %+v", len(steps), steps)
	}
	for _, s := range steps {
		if s.Kind != StepExec {
			t.Errorf("unexpected non-exec step %+v", s)
		}
	}
}

func TestIOSSteps_RedactsSecrets(t *testing.T) {
	cert := writeTemp(t, "dist.p12")
	steps, err := BuildIOSSteps(IOSOptions{
		KeychainName:     "buildkc",
		KeychainPassword: "SUPERSECRET",
		CertPath:         cert,
		CertPassword:     "CERTSECRET",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, s := range steps {
		cmd := s.CommandString()
		if strings.Contains(cmd, "SUPERSECRET") {
			t.Errorf("keychain password leaked in %q", cmd)
		}
		if strings.Contains(cmd, "CERTSECRET") {
			t.Errorf("cert password leaked in %q", cmd)
		}
		// Non-secret tokens must remain visible.
		if s.Args[0] == "create-keychain" {
			if !strings.Contains(cmd, "buildkc") {
				t.Errorf("keychain name redacted in %q", cmd)
			}
			if !strings.Contains(cmd, "<redacted>") {
				t.Errorf("password not redacted in %q", cmd)
			}
		}
	}
}

func TestBuildAndroidSteps(t *testing.T) {
	tests := []struct {
		name      string
		opts      AndroidOptions
		wantErr   string
		wantCmds  []string
		wantCount int
	}{
		{
			name:    "missing sdk root",
			opts:    AndroidOptions{},
			wantErr: "Android SDK root is required",
		},
		{
			name:      "licenses only",
			opts:      AndroidOptions{SDKRoot: "/sdk", AcceptLicenses: true},
			wantCount: 1,
			wantCmds:  []string{"sdkmanager --sdk_root=/sdk --licenses"},
		},
		{
			name: "packages with licenses",
			opts: AndroidOptions{
				SDKRoot:        "/sdk",
				AcceptLicenses: true,
				Packages:       []string{"platform-tools", "platforms;android-34"},
			},
			wantCount: 3,
			wantCmds: []string{
				"sdkmanager --sdk_root=/sdk --licenses",
				"sdkmanager --sdk_root=/sdk platform-tools",
				"sdkmanager --sdk_root=/sdk 'platforms;android-34'",
			},
		},
		{
			name:      "blank packages skipped",
			opts:      AndroidOptions{SDKRoot: "/sdk", Packages: []string{"  ", "platform-tools", ""}},
			wantCount: 1,
			wantCmds:  []string{"sdkmanager --sdk_root=/sdk platform-tools"},
		},
		{
			name:      "no licenses no packages yields empty plan",
			opts:      AndroidOptions{SDKRoot: "/sdk"},
			wantCount: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			steps, err := BuildAndroidSteps(tc.opts)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(steps) != tc.wantCount {
				t.Fatalf("got %d steps, want %d: %+v", len(steps), tc.wantCount, steps)
			}
			var got []string
			for _, s := range steps {
				got = append(got, s.CommandString())
			}
			if tc.wantCmds != nil && !reflect.DeepEqual(got, tc.wantCmds) {
				t.Errorf("commands = %v, want %v", got, tc.wantCmds)
			}
		})
	}
}

func TestDefaultProfilesDir(t *testing.T) {
	got := DefaultProfilesDir("/Users/op")
	want := filepath.Join("/Users/op", "Library", "MobileDevice", "Provisioning Profiles")
	if got != want {
		t.Errorf("DefaultProfilesDir = %q, want %q", got, want)
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"plain", "plain"},
		{"platforms;android-34", "'platforms;android-34'"},
		{"", "''"},
		{"with space", "'with space'"},
		{"/abs/path", "/abs/path"},
	}
	for _, tc := range tests {
		if got := shellQuote(tc.in); got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

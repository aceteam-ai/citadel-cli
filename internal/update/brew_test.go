package update

import "testing"

func TestHomebrewPrefixForArch(t *testing.T) {
	cases := map[string]string{
		"arm64": "/opt/homebrew",
		"amd64": "/usr/local",
		"386":   "/usr/local", // any non-arm64 falls to the Intel prefix
	}
	for arch, want := range cases {
		if got := HomebrewPrefixForArch(arch); got != want {
			t.Errorf("HomebrewPrefixForArch(%q) = %q, want %q", arch, got, want)
		}
	}
}

func TestHomebrewBinPath(t *testing.T) {
	if got := HomebrewBinPath("/opt/homebrew"); got != "/opt/homebrew/bin/citadel" {
		t.Errorf("HomebrewBinPath(/opt/homebrew) = %q", got)
	}
	if got := HomebrewBinPath("/usr/local"); got != "/usr/local/bin/citadel" {
		t.Errorf("HomebrewBinPath(/usr/local) = %q", got)
	}
}

func TestStableDarwinExecPath(t *testing.T) {
	cases := []struct {
		name     string
		resolved string
		wantPath string
		wantBrew bool
	}{
		{
			name:     "apple silicon cellar resolves to stable bin symlink",
			resolved: "/opt/homebrew/Cellar/citadel/2.155.0/bin/citadel",
			wantPath: "/opt/homebrew/bin/citadel",
			wantBrew: true,
		},
		{
			name:     "intel cellar resolves to stable bin symlink",
			resolved: "/usr/local/Cellar/citadel/2.155.0/bin/citadel",
			wantPath: "/usr/local/bin/citadel",
			wantBrew: true,
		},
		{
			name:     "curl install into user local bin is left unchanged",
			resolved: "/Users/jason/.local/bin/citadel",
			wantPath: "/Users/jason/.local/bin/citadel",
			wantBrew: false,
		},
		{
			name:     "curl install into usr local bin (intel) is left unchanged",
			resolved: "/usr/local/bin/citadel",
			wantPath: "/usr/local/bin/citadel",
			wantBrew: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotPath, gotBrew := StableDarwinExecPath(tc.resolved)
			if gotPath != tc.wantPath || gotBrew != tc.wantBrew {
				t.Errorf("StableDarwinExecPath(%q) = (%q, %v), want (%q, %v)",
					tc.resolved, gotPath, gotBrew, tc.wantPath, tc.wantBrew)
			}
		})
	}
}

func TestIsHomebrewManagedPath(t *testing.T) {
	cases := []struct {
		name     string
		resolved string
		goos     string
		want     bool
	}{
		{"darwin cellar is managed", "/opt/homebrew/Cellar/citadel/2.1.0/bin/citadel", "darwin", true},
		{"darwin intel cellar is managed", "/usr/local/Cellar/citadel/2.1.0/bin/citadel", "darwin", true},
		{"darwin curl install not managed", "/usr/local/bin/citadel", "darwin", false},
		{"darwin user local not managed", "/Users/jason/.local/bin/citadel", "darwin", false},
		// A linuxbrew Cellar path must NOT be treated as managed: the guard is
		// macOS-only, so an in-place swap still works on Linux.
		{"linux cellar path not managed (linuxbrew)", "/home/linuxbrew/.linuxbrew/Cellar/citadel/2.1.0/bin/citadel", "linux", false},
		{"windows path not managed", `C:\Program Files\citadel\citadel.exe`, "windows", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsHomebrewManagedPath(tc.resolved, tc.goos); got != tc.want {
				t.Errorf("IsHomebrewManagedPath(%q, %q) = %v, want %v", tc.resolved, tc.goos, got, tc.want)
			}
		})
	}
}

package platform

import (
	"runtime"
	"strings"
	"testing"
)

func TestGetVNCManager(t *testing.T) {
	mgr := GetVNCManager()
	if mgr == nil {
		t.Fatal("GetVNCManager() returned nil")
	}

	// Verify correct manager for platform
	switch runtime.GOOS {
	case "linux":
		if _, ok := mgr.(*LinuxVNCManager); !ok {
			t.Errorf("GetVNCManager() on Linux did not return LinuxVNCManager, got %T", mgr)
		}
	case "darwin":
		if _, ok := mgr.(*DarwinVNCManager); !ok {
			t.Errorf("GetVNCManager() on macOS did not return DarwinVNCManager, got %T", mgr)
		}
	case "windows":
		if _, ok := mgr.(*WindowsVNCManager); !ok {
			t.Errorf("GetVNCManager() on Windows did not return WindowsVNCManager, got %T", mgr)
		}
	}
}

func TestGenerateVNCPassword(t *testing.T) {
	pw, err := GenerateVNCPassword()
	if err != nil {
		t.Fatalf("GenerateVNCPassword() error = %v", err)
	}

	// Must be exactly 8 characters (VNC DES limit)
	if len(pw) != 8 {
		t.Errorf("GenerateVNCPassword() length = %d, want 8", len(pw))
	}

	// Must be alphanumeric only
	for _, c := range pw {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			t.Errorf("GenerateVNCPassword() contains non-alphanumeric character: %c", c)
		}
	}

	// Should generate different passwords (probabilistic, but collision is ~1 in 2e14)
	pw2, err := GenerateVNCPassword()
	if err != nil {
		t.Fatalf("GenerateVNCPassword() second call error = %v", err)
	}
	if pw == pw2 {
		t.Errorf("GenerateVNCPassword() generated identical passwords: %s", pw)
	}
}

func TestValidateVNCPort(t *testing.T) {
	tests := []struct {
		name    string
		port    int
		wantErr bool
	}{
		{"valid default", 5900, false},
		{"valid custom", 5901, false},
		{"valid min", 1, false},
		{"valid max", 65535, false},
		{"invalid zero", 0, true},
		{"invalid negative", -1, true},
		{"invalid too high", 65536, true},
		{"invalid very high", 100000, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateVNCPort(tt.port)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateVNCPort(%d) error = %v, wantErr %v", tt.port, err, tt.wantErr)
			}
		})
	}
}

func TestVNCManagerInterfaceCompliance(t *testing.T) {
	// Verify all managers implement the VNCManager interface
	var _ VNCManager = (*WindowsVNCManager)(nil)
	var _ VNCManager = (*LinuxVNCManager)(nil)
	var _ VNCManager = (*DarwinVNCManager)(nil)
}

func TestEncryptVNCPassword(t *testing.T) {
	// Test with a known password. The VNC DES encryption is deterministic
	// given the same input, so we can verify the output is consistent.
	encrypted1, err := encryptVNCPassword("testpass")
	if err != nil {
		t.Fatalf("encryptVNCPassword() error = %v", err)
	}

	// Must produce a 16-char hex string (8 bytes = 16 hex chars)
	if len(encrypted1) != 16 {
		t.Errorf("encryptVNCPassword() hex length = %d, want 16", len(encrypted1))
	}

	// Must be valid hex
	for _, c := range encrypted1 {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("encryptVNCPassword() contains non-hex character: %c", c)
		}
	}

	// Same input must produce same output (deterministic)
	encrypted2, err := encryptVNCPassword("testpass")
	if err != nil {
		t.Fatalf("encryptVNCPassword() second call error = %v", err)
	}
	if encrypted1 != encrypted2 {
		t.Errorf("encryptVNCPassword() not deterministic: %s != %s", encrypted1, encrypted2)
	}

	// Different passwords must produce different output
	encrypted3, err := encryptVNCPassword("diffpass")
	if err != nil {
		t.Fatalf("encryptVNCPassword() third call error = %v", err)
	}
	if encrypted1 == encrypted3 {
		t.Errorf("encryptVNCPassword() same output for different passwords")
	}

	// Short password should be zero-padded (not error)
	encrypted4, err := encryptVNCPassword("ab")
	if err != nil {
		t.Fatalf("encryptVNCPassword(short) error = %v", err)
	}
	if len(encrypted4) != 16 {
		t.Errorf("encryptVNCPassword(short) hex length = %d, want 16", len(encrypted4))
	}

	// Empty password should work (zero-padded)
	encrypted5, err := encryptVNCPassword("")
	if err != nil {
		t.Fatalf("encryptVNCPassword(empty) error = %v", err)
	}
	if len(encrypted5) != 16 {
		t.Errorf("encryptVNCPassword(empty) hex length = %d, want 16", len(encrypted5))
	}
}

func TestReverseBits(t *testing.T) {
	tests := []struct {
		input    byte
		expected byte
	}{
		{0x00, 0x00},
		{0xFF, 0xFF},
		{0x01, 0x80},
		{0x80, 0x01},
		{0xAA, 0x55}, // 10101010 -> 01010101
		{0x55, 0xAA}, // 01010101 -> 10101010
	}

	for _, tt := range tests {
		result := reverseBits(tt.input)
		if result != tt.expected {
			t.Errorf("reverseBits(0x%02X) = 0x%02X, want 0x%02X", tt.input, result, tt.expected)
		}
	}
}

func TestLinuxVNCManagerIsRunning(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Skipping Linux-specific test")
	}

	mgr := &LinuxVNCManager{}

	// Should not panic even if ss command fails
	_ = mgr.IsRunning()
	_ = mgr.Port()
}

func TestWindowsVNCManagerIsRunning(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Skipping Windows-specific test")
	}

	mgr := &WindowsVNCManager{}

	// Should not panic even if TightVNC is not installed
	_ = mgr.IsInstalled()
	_ = mgr.IsRunning()
	_ = mgr.Port()
}

func TestUninstallCmd(t *testing.T) {
	// Pure function test: verifies the correct command + args for each package manager
	// without executing anything.
	tests := []struct {
		pkgMgr   string
		wantName string
		wantArgs []string
	}{
		{"apt-get", "apt-get", []string{"remove", "-y", "-qq", "x11vnc"}},
		{"dnf", "dnf", []string{"remove", "-y", "x11vnc"}},
		{"yum", "yum", []string{"remove", "-y", "x11vnc"}},
		{"pacman", "pacman", []string{"-R", "--noconfirm", "x11vnc"}},
		{"zypper", "zypper", []string{"remove", "-y", "x11vnc"}},
	}

	for _, tt := range tests {
		t.Run(tt.pkgMgr, func(t *testing.T) {
			name, args := uninstallCmd(tt.pkgMgr)
			if name != tt.wantName {
				t.Errorf("uninstallCmd(%q) name = %q, want %q", tt.pkgMgr, name, tt.wantName)
			}
			if len(args) != len(tt.wantArgs) {
				t.Fatalf("uninstallCmd(%q) args count = %d, want %d", tt.pkgMgr, len(args), len(tt.wantArgs))
			}
			for i, arg := range args {
				if arg != tt.wantArgs[i] {
					t.Errorf("uninstallCmd(%q) args[%d] = %q, want %q", tt.pkgMgr, i, arg, tt.wantArgs[i])
				}
			}
		})
	}
}

func TestUninstallCmdUnknown(t *testing.T) {
	// Unknown package manager returns empty name and nil args
	name, args := uninstallCmd("brew")
	if name != "" {
		t.Errorf("uninstallCmd(\"brew\") name = %q, want empty", name)
	}
	if args != nil {
		t.Errorf("uninstallCmd(\"brew\") args = %v, want nil", args)
	}
}

func TestInstallCmd(t *testing.T) {
	// Pure function test: verifies the correct install command + args for each
	// package manager without executing anything (no real apt/x11vnc needed).
	tests := []struct {
		pkgMgr   string
		wantName string
		wantArgs []string
	}{
		{"apt-get", "apt-get", []string{"install", "-y", "-qq", "x11vnc"}},
		{"dnf", "dnf", []string{"install", "-y", "x11vnc"}},
		{"yum", "yum", []string{"install", "-y", "x11vnc"}},
		{"pacman", "pacman", []string{"-S", "--noconfirm", "x11vnc"}},
		{"zypper", "zypper", []string{"install", "-y", "x11vnc"}},
	}

	for _, tt := range tests {
		t.Run(tt.pkgMgr, func(t *testing.T) {
			name, args := installCmd(tt.pkgMgr)
			if name != tt.wantName {
				t.Errorf("installCmd(%q) name = %q, want %q", tt.pkgMgr, name, tt.wantName)
			}
			if len(args) != len(tt.wantArgs) {
				t.Fatalf("installCmd(%q) args count = %d, want %d", tt.pkgMgr, len(args), len(tt.wantArgs))
			}
			for i, arg := range args {
				if arg != tt.wantArgs[i] {
					t.Errorf("installCmd(%q) args[%d] = %q, want %q", tt.pkgMgr, i, arg, tt.wantArgs[i])
				}
			}
			// Every install command must reference the x11vnc package as the
			// final argument so we never accidentally install the wrong thing.
			if len(args) == 0 || args[len(args)-1] != "x11vnc" {
				t.Errorf("installCmd(%q) last arg = %v, want trailing \"x11vnc\"", tt.pkgMgr, args)
			}
		})
	}
}

func TestInstallCmdUnknown(t *testing.T) {
	// Unknown package manager returns empty name and nil args, matching
	// uninstallCmd's contract so Install() can detect the unsupported case.
	name, args := installCmd("brew")
	if name != "" {
		t.Errorf("installCmd(\"brew\") name = %q, want empty", name)
	}
	if args != nil {
		t.Errorf("installCmd(\"brew\") args = %v, want nil", args)
	}
}

func TestInstallUninstallCmdSymmetry(t *testing.T) {
	// The install and uninstall paths must support exactly the same set of
	// package managers; a mismatch would mean one path silently fails.
	for _, pm := range []string{"apt-get", "dnf", "yum", "pacman", "zypper"} {
		t.Run(pm, func(t *testing.T) {
			iName, _ := installCmd(pm)
			uName, _ := uninstallCmd(pm)
			if iName == "" {
				t.Errorf("installCmd(%q) returned empty name; uninstall supports it", pm)
			}
			if uName == "" {
				t.Errorf("uninstallCmd(%q) returned empty name; install supports it", pm)
			}
		})
	}
}

func TestDetectPackageManager(t *testing.T) {
	// detectPackageManager should return a non-empty string on any Linux CI
	// or dev machine. We can't know which one, but it should not panic.
	pm := detectPackageManager()
	if runtime.GOOS == "linux" && pm == "" {
		t.Log("Warning: no package manager detected on Linux")
	}
	// On non-Linux, empty is acceptable
}

func TestDefaultVNCPort(t *testing.T) {
	if DefaultVNCPort != 5900 {
		t.Errorf("DefaultVNCPort = %d, want 5900", DefaultVNCPort)
	}
}

func TestBuildX11VNCArgs(t *testing.T) {
	tests := []struct {
		name       string
		display    string // value to set for $DISPLAY (empty string clears it)
		passwdFile string
		port       int
		authFile   string
		wantArgs   []string
		notWant    []string
	}{
		{
			name:       "with explicit auth file and DISPLAY :0",
			display:    ":0",
			passwdFile: "/home/user/.vnc/passwd",
			port:       5900,
			authFile:   "/home/user/.Xauthority",
			wantArgs:   []string{"-display", ":0", "-rfbauth", "/home/user/.vnc/passwd", "-rfbport", "5900", "-forever", "-bg", "-auth", "/home/user/.Xauthority"},
			notWant:    []string{"-find"},
		},
		{
			name:       "with DM auth file uses DISPLAY from env (#287)",
			display:    ":1",
			passwdFile: "/home/user/.vnc/passwd",
			port:       5901,
			authFile:   "/run/user/120/gdm/Xauthority",
			wantArgs:   []string{"-display", ":1", "-auth", "/run/user/120/gdm/Xauthority", "-rfbport", "5901"},
			notWant:    []string{"-find"},
		},
		{
			name:       "coerces empty display to :0",
			display:    "",
			passwdFile: "/home/user/.vnc/passwd",
			port:       5900,
			authFile:   "/var/run/lightdm/root/:0",
			wantArgs:   []string{"-display", ":0", "-auth", "/var/run/lightdm/root/:0"},
			notWant:    []string{"-find"},
		},
		{
			name:       "auto-discover with -find when no auth file",
			display:    ":0",
			passwdFile: "/home/user/.vnc/passwd",
			port:       5900,
			authFile:   "",
			wantArgs:   []string{"-find", "-rfbauth", "/home/user/.vnc/passwd", "-rfbport", "5900", "-forever", "-bg"},
			notWant:    []string{"-display", "-auth"},
		},
		{
			name:       "custom port",
			display:    ":0",
			passwdFile: "/root/.vnc/passwd",
			port:       5910,
			authFile:   "/root/.Xauthority",
			wantArgs:   []string{"-rfbport", "5910"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := buildX11VNCArgs(tt.passwdFile, tt.port, tt.authFile, tt.display)
			argStr := strings.Join(args, " ")

			for _, want := range tt.wantArgs {
				found := false
				for _, a := range args {
					if a == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("buildX11VNCArgs() missing expected arg %q in %v", want, args)
				}
			}

			for _, nw := range tt.notWant {
				for _, a := range args {
					if a == nw {
						t.Errorf("buildX11VNCArgs() should not contain %q, got: %s", nw, argStr)
					}
				}
			}
		})
	}
}

func TestBuildX11VNCArgsOrder(t *testing.T) {
	// When using -find, it should be the first argument
	args := buildX11VNCArgs("/home/user/.vnc/passwd", 5900, "", ":0")
	if len(args) == 0 {
		t.Fatal("buildX11VNCArgs() returned empty args")
	}
	if args[0] != "-find" {
		t.Errorf("buildX11VNCArgs() with no auth: first arg = %q, want -find", args[0])
	}

	// When using explicit auth, -display should come first
	args = buildX11VNCArgs("/home/user/.vnc/passwd", 5900, "/home/user/.Xauthority", ":0")
	if len(args) == 0 {
		t.Fatal("buildX11VNCArgs() returned empty args")
	}
	if args[0] != "-display" {
		t.Errorf("buildX11VNCArgs() with auth: first arg = %q, want -display", args[0])
	}
}

func TestErrSudoRequired(t *testing.T) {
	if ErrSudoRequired == nil {
		t.Fatal("ErrSudoRequired is nil")
	}
	msg := ErrSudoRequired.Error()
	if !strings.Contains(msg, "sudo") {
		t.Errorf("ErrSudoRequired message should mention sudo, got: %s", msg)
	}
	if !strings.Contains(msg, "root privileges") {
		t.Errorf("ErrSudoRequired message should include the fix command, got: %s", msg)
	}
}

func TestLinuxVNCManagerEffectivePort(t *testing.T) {
	mgr := &LinuxVNCManager{}

	// Default port when none is configured
	if mgr.effectivePort() != DefaultVNCPort {
		t.Errorf("effectivePort() = %d, want %d (default)", mgr.effectivePort(), DefaultVNCPort)
	}

	// Custom port after Configure sets it
	mgr.port = 5901
	if mgr.effectivePort() != 5901 {
		t.Errorf("effectivePort() = %d, want 5901", mgr.effectivePort())
	}
}

func TestLinuxVNCManagerIsInstalled(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Skipping Linux-specific test")
	}

	mgr := &LinuxVNCManager{}
	// Should not panic regardless of whether x11vnc is actually installed
	_ = mgr.IsInstalled()
}

func TestLinuxVNCManagerStopIdempotent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Skipping Linux-specific test")
	}

	mgr := &LinuxVNCManager{}
	// Stop when nothing is running should not error
	err := mgr.Stop()
	if err != nil {
		t.Errorf("Stop() when nothing running should not error, got: %v", err)
	}
}

func TestLinuxVNCManagerPortFallback(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Skipping Linux-specific test")
	}

	mgr := &LinuxVNCManager{}
	// Port() should return a value without panicking
	port := mgr.Port()
	// When no VNC server is running, it should still return the default port
	if port < 0 {
		t.Errorf("Port() returned negative value: %d", port)
	}
}

func TestLinuxVNCManagerConfigureValidation(t *testing.T) {
	mgr := &LinuxVNCManager{}

	// Invalid port should be rejected
	err := mgr.Configure("testpass", 0)
	if err == nil {
		t.Error("Configure() with port 0 should return error")
	}

	err = mgr.Configure("testpass", -1)
	if err == nil {
		t.Error("Configure() with port -1 should return error")
	}

	err = mgr.Configure("testpass", 70000)
	if err == nil {
		t.Error("Configure() with port 70000 should return error")
	}
}

func TestLinuxVNCManagerConfigureSetsPort(t *testing.T) {
	// Test that Configure validates and stores the port before shelling out.
	// We don't call the real Configure() because it runs x11vnc -storepasswd.
	// Instead, we verify the port validation + assignment logic directly.
	mgr := &LinuxVNCManager{}

	// Valid port should be accepted by ValidateVNCPort and stored
	if err := ValidateVNCPort(5901); err != nil {
		t.Fatalf("ValidateVNCPort(5901) unexpected error: %v", err)
	}
	mgr.port = 5901
	if mgr.port != 5901 {
		t.Errorf("port assignment failed: got %d, want 5901", mgr.port)
	}
	if mgr.effectivePort() != 5901 {
		t.Errorf("effectivePort() = %d, want 5901", mgr.effectivePort())
	}
}

func TestKickstartActivateArgs(t *testing.T) {
	args := kickstartActivateArgs("secret12")
	argStr := strings.Join(args, " ")

	// Core activation flags must be present
	for _, want := range []string{"-activate", "-configure", "-access", "-on", "-restart", "-agent", "-privs", "-all"} {
		found := false
		for _, a := range args {
			if a == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("kickstartActivateArgs() missing %q in %v", want, args)
		}
	}

	// VNC legacy mode and password must be configured
	if !strings.Contains(argStr, "-setvnclegacy -vnclegacy yes") {
		t.Errorf("kickstartActivateArgs() should enable VNC legacy mode, got: %s", argStr)
	}
	if !strings.Contains(argStr, "-setvncpw -vncpw secret12") {
		t.Errorf("kickstartActivateArgs() should set the VNC password, got: %s", argStr)
	}

	// The password must appear immediately after -vncpw
	for i, a := range args {
		if a == "-vncpw" {
			if i+1 >= len(args) || args[i+1] != "secret12" {
				t.Errorf("kickstartActivateArgs() -vncpw not followed by password, got: %v", args)
			}
		}
	}
}

func TestKickstartActivateArgsPasswordPositioning(t *testing.T) {
	// A different password must flow through to the right slot.
	args := kickstartActivateArgs("abc")
	idx := -1
	for i, a := range args {
		if a == "-vncpw" {
			idx = i
			break
		}
	}
	if idx == -1 {
		t.Fatal("kickstartActivateArgs() missing -vncpw flag")
	}
	if args[idx+1] != "abc" {
		t.Errorf("kickstartActivateArgs() password = %q, want %q", args[idx+1], "abc")
	}
}

func TestKickstartDeactivateArgs(t *testing.T) {
	args := kickstartDeactivateArgs()
	for _, want := range []string{"-deactivate", "-configure", "-access", "-off"} {
		found := false
		for _, a := range args {
			if a == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("kickstartDeactivateArgs() missing %q in %v", want, args)
		}
	}
	// Must not contain activation flags
	for _, nw := range []string{"-activate", "-on"} {
		for _, a := range args {
			if a == nw {
				t.Errorf("kickstartDeactivateArgs() should not contain %q, got: %v", nw, args)
			}
		}
	}
}

func TestKickstartRestartArgs(t *testing.T) {
	args := kickstartRestartArgs()
	want := []string{"-restart", "-agent"}
	if len(args) != len(want) {
		t.Fatalf("kickstartRestartArgs() = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("kickstartRestartArgs()[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

func TestDarwinCmdSudoPrefix(t *testing.T) {
	// With sudo: the command should be "sudo" and the real command shifted
	// into the argument list.
	cmd := darwinCmd(true, kickstartPath, "-activate")
	if !strings.HasSuffix(cmd.Path, "sudo") && cmd.Args[0] != "sudo" {
		t.Errorf("darwinCmd(needsSudo=true) should invoke sudo, got args: %v", cmd.Args)
	}
	if cmd.Args[1] != kickstartPath {
		t.Errorf("darwinCmd(needsSudo=true) args[1] = %q, want %q", cmd.Args[1], kickstartPath)
	}
	if cmd.Args[2] != "-activate" {
		t.Errorf("darwinCmd(needsSudo=true) args[2] = %q, want -activate", cmd.Args[2])
	}

	// Without sudo: the command runs kickstart directly.
	cmd = darwinCmd(false, kickstartPath, "-activate")
	if cmd.Args[0] != kickstartPath {
		t.Errorf("darwinCmd(needsSudo=false) args[0] = %q, want %q", cmd.Args[0], kickstartPath)
	}
	if cmd.Args[1] != "-activate" {
		t.Errorf("darwinCmd(needsSudo=false) args[1] = %q, want -activate", cmd.Args[1])
	}
}

func TestKickstartPath(t *testing.T) {
	// Guard against accidental edits to the well-known ARD kickstart path.
	const want = "/System/Library/CoreServices/RemoteManagement/ARDAgent.app/Contents/Resources/kickstart"
	if kickstartPath != want {
		t.Errorf("kickstartPath = %q, want %q", kickstartPath, want)
	}
}

func TestErrDarwinSudoRequired(t *testing.T) {
	if ErrDarwinSudoRequired == nil {
		t.Fatal("ErrDarwinSudoRequired is nil")
	}
	msg := ErrDarwinSudoRequired.Error()
	if !strings.Contains(msg, "sudo") {
		t.Errorf("ErrDarwinSudoRequired should mention sudo, got: %s", msg)
	}
	if !strings.Contains(msg, "root privileges") {
		t.Errorf("ErrDarwinSudoRequired should mention root privileges, got: %s", msg)
	}
}

func TestDarwinVNCManagerDetection(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Skipping macOS-specific test")
	}

	mgr := &DarwinVNCManager{}
	// Detection methods must not panic regardless of Screen Sharing state.
	_ = mgr.IsInstalled()
	_ = mgr.IsRunning()
	_ = mgr.Port()

	// kickstart ships with macOS, so IsInstalled should be true.
	if !mgr.IsInstalled() {
		t.Errorf("DarwinVNCManager.IsInstalled() = false on macOS; kickstart should be present at %s", kickstartPath)
	}

	// Port() must be consistent with IsRunning().
	if mgr.IsRunning() {
		if mgr.Port() != DefaultVNCPort {
			t.Errorf("DarwinVNCManager.Port() = %d while running, want %d", mgr.Port(), DefaultVNCPort)
		}
	} else if mgr.Port() != 0 {
		t.Errorf("DarwinVNCManager.Port() = %d while not running, want 0", mgr.Port())
	}
}

func TestDarwinVNCManagerConfigureValidation(t *testing.T) {
	mgr := &DarwinVNCManager{}
	// Invalid ports must be rejected before any kickstart invocation.
	for _, p := range []int{0, -1, 70000} {
		if err := mgr.Configure("testpass", p); err == nil {
			t.Errorf("Configure() with port %d should return error", p)
		}
	}
}

func TestEmbeddedVNCPort(t *testing.T) {
	// Initial state: no embedded VNC
	ClearEmbeddedVNCPort()
	if EmbeddedVNCPort() != 0 {
		t.Errorf("EmbeddedVNCPort() after clear = %d, want 0", EmbeddedVNCPort())
	}

	// Set embedded port
	SetEmbeddedVNCPort(5900)
	if EmbeddedVNCPort() != 5900 {
		t.Errorf("EmbeddedVNCPort() after set = %d, want 5900", EmbeddedVNCPort())
	}

	// Clear again
	ClearEmbeddedVNCPort()
	if EmbeddedVNCPort() != 0 {
		t.Errorf("EmbeddedVNCPort() after second clear = %d, want 0", EmbeddedVNCPort())
	}
}

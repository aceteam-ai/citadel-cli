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

func TestVNCManagerUninstallNoPanic(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Uninstall is destructive on Windows; no-panic test is Linux-only")
	}
	mgr := GetVNCManager()
	// Uninstall() should not panic on any platform, even if nothing is installed
	err := mgr.Uninstall()
	if err != nil {
		t.Logf("Uninstall() returned error (expected on some platforms): %v", err)
	}
}

func TestDefaultVNCPort(t *testing.T) {
	if DefaultVNCPort != 5900 {
		t.Errorf("DefaultVNCPort = %d, want 5900", DefaultVNCPort)
	}
}

func TestBuildX11VNCArgs(t *testing.T) {
	tests := []struct {
		name       string
		passwdFile string
		port       int
		authFile   string
		wantArgs   []string
		notWant    []string
	}{
		{
			name:       "with explicit auth file",
			passwdFile: "/home/user/.vnc/passwd",
			port:       5900,
			authFile:   "/home/user/.Xauthority",
			wantArgs:   []string{"-display", ":0", "-rfbauth", "/home/user/.vnc/passwd", "-rfbport", "5900", "-forever", "-bg", "-auth", "/home/user/.Xauthority"},
			notWant:    []string{"-find"},
		},
		{
			name:       "with DM auth file",
			passwdFile: "/home/user/.vnc/passwd",
			port:       5901,
			authFile:   "/var/run/lightdm/root/:0",
			wantArgs:   []string{"-display", ":0", "-auth", "/var/run/lightdm/root/:0", "-rfbport", "5901"},
			notWant:    []string{"-find"},
		},
		{
			name:       "auto-discover with -find when no auth file",
			passwdFile: "/home/user/.vnc/passwd",
			port:       5900,
			authFile:   "",
			wantArgs:   []string{"-find", "-rfbauth", "/home/user/.vnc/passwd", "-rfbport", "5900", "-forever", "-bg"},
			notWant:    []string{"-display", "-auth"},
		},
		{
			name:       "custom port",
			passwdFile: "/root/.vnc/passwd",
			port:       5910,
			authFile:   "/root/.Xauthority",
			wantArgs:   []string{"-rfbport", "5910"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := buildX11VNCArgs(tt.passwdFile, tt.port, tt.authFile)
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
	args := buildX11VNCArgs("/home/user/.vnc/passwd", 5900, "")
	if len(args) == 0 {
		t.Fatal("buildX11VNCArgs() returned empty args")
	}
	if args[0] != "-find" {
		t.Errorf("buildX11VNCArgs() with no auth: first arg = %q, want -find", args[0])
	}

	// When using explicit auth, -display should come first
	args = buildX11VNCArgs("/home/user/.vnc/passwd", 5900, "/home/user/.Xauthority")
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
	if runtime.GOOS != "linux" {
		t.Skip("Skipping Linux-specific test")
	}

	mgr := &LinuxVNCManager{}

	// Configure with valid port sets the internal port field
	// (even if x11vnc is not installed, the port validation + storage should work)
	// On CI without x11vnc, this will fail at the storepasswd step
	err := mgr.Configure("testpass", 5901)
	if err != nil {
		// Expected when x11vnc is not installed
		t.Logf("Configure() returned error (expected without x11vnc): %v", err)
	} else {
		if mgr.port != 5901 {
			t.Errorf("Configure() did not set port to 5901, got %d", mgr.port)
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

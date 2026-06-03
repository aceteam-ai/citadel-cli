package platform

import (
	"runtime"
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

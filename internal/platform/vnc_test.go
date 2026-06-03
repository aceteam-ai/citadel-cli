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

func TestDefaultVNCPort(t *testing.T) {
	if DefaultVNCPort != 5900 {
		t.Errorf("DefaultVNCPort = %d, want 5900", DefaultVNCPort)
	}
}

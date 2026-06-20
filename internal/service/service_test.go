package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestServiceConfig_Validate(t *testing.T) {
	// Create a temp file to act as a fake binary.
	tmpDir := t.TempDir()
	fakeBin := filepath.Join(tmpDir, "citadel")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		cfg     ServiceConfig
		wantErr bool
	}{
		{
			name:    "empty ExecPath",
			cfg:     ServiceConfig{},
			wantErr: true,
		},
		{
			name:    "relative ExecPath",
			cfg:     ServiceConfig{ExecPath: "citadel"},
			wantErr: true,
		},
		{
			name:    "nonexistent ExecPath",
			cfg:     ServiceConfig{ExecPath: "/nonexistent/path/citadel"},
			wantErr: true,
		},
		{
			name: "valid config",
			cfg: ServiceConfig{
				ExecPath:    fakeBin,
				Args:        []string{"work"},
				Description: "test",
			},
			wantErr: false,
		},
		{
			name: "valid config fills default description",
			cfg: ServiceConfig{
				ExecPath: fakeBin,
				Args:     []string{"work"},
			},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
			// If valid and no description was set, it should be filled.
			if !tc.wantErr && tc.cfg.Description == "" {
				t.Error("expected Description to be set to default after Validate")
			}
		})
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg, err := DefaultConfig()
	if err != nil {
		t.Fatalf("DefaultConfig() error: %v", err)
	}
	if cfg.ExecPath == "" {
		t.Error("expected ExecPath to be non-empty")
	}
	if !filepath.IsAbs(cfg.ExecPath) {
		t.Errorf("expected absolute ExecPath, got %q", cfg.ExecPath)
	}
	if len(cfg.Args) == 0 || cfg.Args[0] != "work" {
		t.Errorf("expected Args to start with 'work', got %v", cfg.Args)
	}
	if cfg.Description == "" {
		t.Error("expected Description to be set")
	}
}

func TestNewManager(t *testing.T) {
	mgr := NewManager()
	if mgr == nil {
		t.Fatal("NewManager() returned nil")
	}
}

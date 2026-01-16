package worker

import (
	"testing"
	"time"
)

func TestNewNexusSource(t *testing.T) {
	tests := []struct {
		name         string
		config       NexusSourceConfig
		wantURL      string
		wantInterval time.Duration
	}{
		{
			name: "with defaults",
			config: NexusSourceConfig{
				NexusURL: "https://nexus.example.com",
			},
			wantURL:      "https://nexus.example.com",
			wantInterval: 5 * time.Second,
		},
		{
			name: "with custom interval",
			config: NexusSourceConfig{
				NexusURL:     "https://nexus.example.com",
				PollInterval: 10 * time.Second,
			},
			wantURL:      "https://nexus.example.com",
			wantInterval: 10 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := NewNexusSource(tt.config)

			if source == nil {
				t.Fatal("NewNexusSource returned nil")
			}
			if source.nexusURL != tt.wantURL {
				t.Errorf("nexusURL = %v, want %v", source.nexusURL, tt.wantURL)
			}
			if source.pollInterval != tt.wantInterval {
				t.Errorf("pollInterval = %v, want %v", source.pollInterval, tt.wantInterval)
			}
		})
	}
}

func TestNexusSourceName(t *testing.T) {
	source := NewNexusSource(NexusSourceConfig{
		NexusURL: "https://nexus.example.com",
	})

	if source.Name() != "nexus" {
		t.Errorf("Name() = %v, want nexus", source.Name())
	}
}

func TestNexusSourceClose(t *testing.T) {
	source := NewNexusSource(NexusSourceConfig{
		NexusURL: "https://nexus.example.com",
	})

	// Close without connecting should not error
	if err := source.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}

	// Close with ticker should stop it
	source.ticker = time.NewTicker(1 * time.Second)
	if err := source.Close(); err != nil {
		t.Errorf("Close() with ticker error = %v, want nil", err)
	}
}

func TestNexusSourceImplementsJobSource(t *testing.T) {
	var _ JobSource = (*NexusSource)(nil)
}

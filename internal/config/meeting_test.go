package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultMeeting(t *testing.T) {
	m := DefaultMeeting()
	if !m.MeetingEnabled {
		t.Errorf("DefaultMeeting should be enabled (opt-out), got %+v", m)
	}
	// Streaming (issue #5435) is also opt-out with sane cadence defaults.
	if !m.StreamingEnabled {
		t.Errorf("DefaultMeeting should have streaming enabled (opt-out), got %+v", m)
	}
	if m.StreamingInterval() != defaultStreamingIntervalSeconds*time.Second {
		t.Errorf("StreamingInterval = %v, want %v", m.StreamingInterval(), defaultStreamingIntervalSeconds*time.Second)
	}
	if m.StreamingWindow() != defaultStreamingWindowSeconds*time.Second {
		t.Errorf("StreamingWindow = %v, want %v", m.StreamingWindow(), defaultStreamingWindowSeconds*time.Second)
	}
}

func TestStreamingDurationFallbacks(t *testing.T) {
	// A persisted config with non-positive values (hand-edited or truncated)
	// must not yield a zero ticker interval; the accessor clamps to defaults.
	m := &Meeting{StreamingIntervalSeconds: 0, StreamingWindowSeconds: -5}
	if m.StreamingInterval() != defaultStreamingIntervalSeconds*time.Second {
		t.Errorf("StreamingInterval fallback = %v, want default", m.StreamingInterval())
	}
	if m.StreamingWindow() != defaultStreamingWindowSeconds*time.Second {
		t.Errorf("StreamingWindow fallback = %v, want default", m.StreamingWindow())
	}
	// Explicit positive values are honored.
	m2 := &Meeting{StreamingIntervalSeconds: 5, StreamingWindowSeconds: 3}
	if m2.StreamingInterval() != 5*time.Second {
		t.Errorf("StreamingInterval = %v, want 5s", m2.StreamingInterval())
	}
	if m2.StreamingWindow() != 3*time.Second {
		t.Errorf("StreamingWindow = %v, want 3s", m2.StreamingWindow())
	}
}

func TestLoadMeeting_StreamingDefaultsPreservedOnPartialFile(t *testing.T) {
	// A file that only sets meeting_enabled must keep the streaming defaults
	// (absent keys preserve defaults via unmarshal-into-defaults).
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "meeting.yaml"), []byte("meeting_enabled: true\n"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	m := LoadMeeting(dir)
	if !m.StreamingEnabled {
		t.Errorf("partial file should preserve StreamingEnabled default (true), got %+v", m)
	}
	if m.StreamingIntervalSeconds != defaultStreamingIntervalSeconds {
		t.Errorf("partial file should preserve interval default, got %d", m.StreamingIntervalSeconds)
	}
}

func TestLoadMeeting_NoFile(t *testing.T) {
	dir := t.TempDir()
	m := LoadMeeting(dir)
	if !m.MeetingEnabled {
		t.Errorf("LoadMeeting with no file should default to enabled, got %+v", m)
	}
}

func TestMeetingSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	original := &Meeting{MeetingEnabled: false}

	if err := SaveMeeting(dir, original); err != nil {
		t.Fatalf("SaveMeeting: %v", err)
	}

	loaded := LoadMeeting(dir)
	if loaded.MeetingEnabled != original.MeetingEnabled {
		t.Errorf("round trip mismatch: saved %+v, loaded %+v", original, loaded)
	}
}

func TestLoadMeeting_DisabledFromFile(t *testing.T) {
	dir := t.TempDir()
	data := []byte("meeting_enabled: false\n")
	if err := os.WriteFile(filepath.Join(dir, "meeting.yaml"), data, 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	m := LoadMeeting(dir)
	if m.MeetingEnabled {
		t.Error("meeting_enabled should be false from file")
	}
}

func TestLoadMeeting_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	data := []byte("not: [valid: yaml: {{{")
	if err := os.WriteFile(filepath.Join(dir, "meeting.yaml"), data, 0644); err != nil {
		t.Fatalf("write invalid yaml: %v", err)
	}

	m := LoadMeeting(dir)
	if !m.MeetingEnabled {
		t.Errorf("invalid YAML should return defaults (enabled), got %+v", m)
	}
}

func TestSaveMeeting_CreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	if err := SaveMeeting(dir, DefaultMeeting()); err != nil {
		t.Fatalf("SaveMeeting should create nested dirs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "meeting.yaml")); err != nil {
		t.Errorf("meeting file should exist after save: %v", err)
	}
}

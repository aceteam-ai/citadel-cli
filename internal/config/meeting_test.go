package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultMeeting(t *testing.T) {
	m := DefaultMeeting()
	if !m.MeetingEnabled {
		t.Errorf("DefaultMeeting should be enabled (opt-out), got %+v", m)
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

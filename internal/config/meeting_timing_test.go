package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/meetingpolicy"
)

func TestMeetingTimingConfigDefaultsAndStrictYAML(t *testing.T) {
	dir := t.TempDir()
	p, err := LoadMeeting(dir).TimingProfile()
	if err != nil || p != meetingpolicy.Default() {
		t.Fatal(p, err)
	}
	for _, body := range []string{"safety:\n  setup_seconds: 1.5\n", "safety:\n  admission_max_seconds: 601\n", "safety: [invalid]\n", "safety:\n  cleanup_seconds: 9223372036854775808\n", "safety: {setup_seconds: 1, setup_seconds: 2}\n"} {
		if err := os.WriteFile(filepath.Join(dir, meetingFile), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadMeeting(dir).TimingProfile(); err == nil {
			t.Fatal("malformed policy defaulted")
		}
	}
	if err := os.WriteFile(filepath.Join(dir, meetingFile), []byte("safety:\n  transcription_seconds: 60\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err = LoadMeeting(dir).TimingProfile()
	if err != nil || p.Transcription != 60 || p.AdmissionMax != 600 {
		t.Fatal(p, err)
	}
}
func TestMeetingTimingUnreadableConfigCannotBecomeDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, meetingFile), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMeeting(dir).TimingProfile(); err == nil {
		t.Fatal("unreadable policy became ready")
	}
}
func TestMeetingTimingSaveRejectsBeforeEffects(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	m := DefaultMeeting()
	m.Safety = meetingpolicy.Config{"cleanup_seconds": "181"}
	if err := SaveMeeting(dir, m); err == nil {
		t.Fatal("expanded policy saved")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("invalid policy made host effects")
	}
}

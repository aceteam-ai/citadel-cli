package jobs

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestStartServices_DockerCLIMissing_RefusesWithFriendlyMessage covers the
// onboarding autostart path (citadel #767 follow-up): APPLY_DEVICE_CONFIG's
// startServices is the fresh-node first-start path (citadel init -> wizard ->
// autoStartServices), the exact scenario the issue reports. With no docker on
// PATH, it must refuse with the friendly platform.PreflightDockerStart
// diagnosis -- never a raw `exec: "docker": executable file not found in
// $PATH` string -- and must refuse before ever touching a compose file (no
// service dirs exist here, so any attempt to exec compose would fail
// differently, proving the preflight is what's stopping this).
func TestStartServices_DockerCLIMissing_RefusesWithFriendlyMessage(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no docker, no native binaries

	h := NewConfigHandler(t.TempDir())
	err := h.startServices(h.ConfigDir, []string{"ollama"})
	if err == nil {
		t.Fatalf("expected startServices to refuse with docker missing from PATH")
	}
	if !strings.Contains(err.Error(), "docker CLI not found on PATH") {
		t.Fatalf("expected friendly docker-missing diagnosis, got: %v", err)
	}
	if strings.Contains(err.Error(), "exec:") {
		t.Fatalf("error must not leak the raw exec error, got: %v", err)
	}
}

// TestDeviceConfig_MeetingEnabledUnmarshal verifies the *bool distinguishes an
// absent field (nil -> leave the persisted toggle untouched) from an explicit
// false (opt out). A plain bool would collapse both to false and silently opt
// every node out whenever any device config is applied.
func TestDeviceConfig_MeetingEnabledUnmarshal(t *testing.T) {
	t.Run("absent -> nil", func(t *testing.T) {
		var c DeviceConfig
		if err := json.Unmarshal([]byte(`{"deviceName":"n"}`), &c); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if c.MeetingEnabled != nil {
			t.Errorf("absent meetingEnabled should be nil, got %v", *c.MeetingEnabled)
		}
	})

	t.Run("explicit false -> non-nil false", func(t *testing.T) {
		var c DeviceConfig
		if err := json.Unmarshal([]byte(`{"meetingEnabled":false}`), &c); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if c.MeetingEnabled == nil {
			t.Fatal("explicit meetingEnabled:false should be non-nil")
		}
		if *c.MeetingEnabled {
			t.Errorf("explicit meetingEnabled:false should be false, got true")
		}
	})

	t.Run("explicit true -> non-nil true", func(t *testing.T) {
		var c DeviceConfig
		if err := json.Unmarshal([]byte(`{"meetingEnabled":true}`), &c); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if c.MeetingEnabled == nil || !*c.MeetingEnabled {
			t.Errorf("explicit meetingEnabled:true should be non-nil true, got %v", c.MeetingEnabled)
		}
	})
}

// TestDeviceConfig_AudioBackupUnmarshal verifies the audio-backup + retention
// fields carry the same absent(nil)-vs-explicit pointer semantics, so applying
// a device config that omits them leaves the persisted values untouched.
func TestDeviceConfig_AudioBackupUnmarshal(t *testing.T) {
	t.Run("absent -> nil", func(t *testing.T) {
		var c DeviceConfig
		if err := json.Unmarshal([]byte(`{"deviceName":"n"}`), &c); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if c.AudioBackupEnabled != nil {
			t.Errorf("absent audioBackupEnabled should be nil, got %v", *c.AudioBackupEnabled)
		}
		if c.MeetingRetentionDays != nil {
			t.Errorf("absent meetingRetentionDays should be nil, got %v", *c.MeetingRetentionDays)
		}
	})

	t.Run("explicit values", func(t *testing.T) {
		var c DeviceConfig
		if err := json.Unmarshal([]byte(`{"audioBackupEnabled":false,"meetingRetentionDays":7}`), &c); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if c.AudioBackupEnabled == nil || *c.AudioBackupEnabled {
			t.Errorf("audioBackupEnabled:false should be non-nil false, got %v", c.AudioBackupEnabled)
		}
		if c.MeetingRetentionDays == nil || *c.MeetingRetentionDays != 7 {
			t.Errorf("meetingRetentionDays:7 should be non-nil 7, got %v", c.MeetingRetentionDays)
		}
	})
}

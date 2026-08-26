package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

// TestUpdateManifest_PreservesPinnedServices pins the fix for citadel#850:
// CitadelManifest here (the round-trip type updateManifest unmarshals the
// whole citadel.yaml into and marshals back out) previously had no
// PinnedServices field, unlike cmd/manifest.go's CitadelManifest -- so ANY
// node's `pinned_services:` allowlist was silently DROPPED the next time
// APPLY_DEVICE_CONFIG ran (dashboard/onboarding config save), un-pinning a
// service and making it preemptible (breaking #577 preemption / #832
// reservations). A device config that doesn't mention pins at all -- the
// normal onboarding-wizard shape -- must leave the existing pins untouched,
// per this struct's own doc contract.
func TestUpdateManifest_PreservesPinnedServices(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "citadel.yaml")

	initial := `node:
  name: existing-node
  tags: [gpu]
pinned_services:
  - bonsai
  - vllm
services:
  - name: bonsai
    compose_file: ./services/bonsai.yml
`
	if err := os.WriteFile(manifestPath, []byte(initial), 0600); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}

	h := NewConfigHandler(dir)
	// A device config that does not mention pinned_services at all -- the
	// shape APPLY_DEVICE_CONFIG normally receives from the onboarding wizard.
	config := &DeviceConfig{DeviceName: "existing-node"}
	if err := h.updateManifest(dir, config); err != nil {
		t.Fatalf("updateManifest: %v", err)
	}

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	var got CitadelManifest
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal round-tripped manifest: %v", err)
	}

	want := []string{"bonsai", "vllm"}
	if !reflect.DeepEqual(got.PinnedServices, want) {
		t.Fatalf("pinned_services did not survive the APPLY_DEVICE_CONFIG round-trip: got %v, want %v", got.PinnedServices, want)
	}

	// Also assert the raw YAML still carries the key, in case a future zero-value
	// change to the field type silently produces a DeepEqual-but-omitted encode.
	if !strings.Contains(string(data), "pinned_services:") {
		t.Fatalf("round-tripped manifest is missing the pinned_services key entirely:\n%s", data)
	}
}

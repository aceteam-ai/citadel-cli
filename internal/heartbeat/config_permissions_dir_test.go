package heartbeat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	citadelconfig "github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// TestConfigPathsWriteTheWorkerPermissionsDir: a direct-Redis config push
// (queue or Pub/Sub) writes permissions where the worker reads them, not the
// handler's legacy invoker-scoped location.
func TestConfigPathsWriteTheWorkerPermissionsDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUDO_USER", "")
	push := func(t *testing.T, h *jobs.ConfigHandler) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"cobrowsePublicOnly": true, "filesEnabled": true})
		if _, err := h.Execute(jobs.JobContext{}, &nexus.Job{Type: "APPLY_DEVICE_CONFIG", Payload: map[string]string{"config": string(payload)}}); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	check := func(t *testing.T, dir string) {
		t.Helper()
		p := citadelconfig.LoadPermissions(dir)
		if !p.CobrowsePublicOnly || !p.Files {
			t.Fatalf("worker permissions dir not updated: %+v", p)
		}
		if _, err := os.Stat(filepath.Join(home, ".citadel-cli", "permissions.yaml")); err == nil {
			t.Fatal("the legacy invoker-scoped permissions file was written")
		}
	}

	t.Run("queue consumer", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "node")
		c, err := NewConfigConsumer(ConfigConsumerConfig{RedisURL: "redis://localhost:6379", ConfigDir: dir, PermissionsDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		defer c.client.Close()
		if c.configHandler.PermissionsDir != dir {
			t.Fatalf("PermissionsDir = %q", c.configHandler.PermissionsDir)
		}
		push(t, c.configHandler)
		check(t, dir)
	})
	t.Run("pubsub subscriber", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "node")
		s, err := NewConfigSubscriber(ConfigSubscriberConfig{RedisURL: "redis://localhost:6379", NodeID: "n", ConfigDir: dir, PermissionsDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		defer s.client.Close()
		if s.configHandler.PermissionsDir != dir {
			t.Fatalf("PermissionsDir = %q", s.configHandler.PermissionsDir)
		}
		push(t, s.configHandler)
		check(t, dir)
	})
}

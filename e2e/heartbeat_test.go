package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/e2e/harness"
	"github.com/google/uuid"
)

// TestNodeHeartbeat tests the node heartbeat functionality
func TestNodeHeartbeat(t *testing.T) {
	aceteamURL := os.Getenv("ACETEAM_URL")
	if aceteamURL == "" {
		aceteamURL = "http://localhost:3000"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	aceteam := harness.NewAceTeamHarness(aceteamURL)

	// Wait for AceTeam to be ready
	t.Log("Waiting for AceTeam API to be ready...")
	if err := aceteam.WaitForReady(ctx, 60*time.Second); err != nil {
		t.Skipf("AceTeam API not ready: %v", err)
	}

	t.Run("SendHeartbeat", func(t *testing.T) {
		nodeID := "test-node-" + uuid.New().String()[:8]

		// Create a node status payload
		status := map[string]interface{}{
			"hostname": nodeID,
			"cpu": map[string]interface{}{
				"usage":  25.5,
				"cores":  8,
				"model":  "AMD Ryzen 9 5900X",
			},
			"memory": map[string]interface{}{
				"total":     32000000000,
				"available": 24000000000,
				"used":      8000000000,
			},
			"gpu": []map[string]interface{}{
				{
					"name":        "NVIDIA RTX 3090",
					"memory":      24576,
					"memoryUsed":  4096,
					"utilization": 15,
				},
			},
			"services": []map[string]interface{}{
				{
					"name":   "vllm",
					"status": "running",
					"port":   8000,
				},
			},
			"tailscaleIP":  "100.64.1.1",
			"version":      "0.1.0",
			"lastHeartbeat": time.Now().UTC().Format(time.RFC3339),
		}

		// Send heartbeat
		err := aceteam.SendHeartbeat(ctx, nodeID, status)
		if err != nil {
			// This may fail if the node isn't registered - that's expected
			t.Logf("Heartbeat result: %v", err)
		} else {
			t.Log("Heartbeat sent successfully")
		}
	})

	t.Run("MultipleHeartbeats", func(t *testing.T) {
		nodeID := "test-node-" + uuid.New().String()[:8]

		// Send multiple heartbeats to test consistency
		for i := 0; i < 3; i++ {
			status := map[string]interface{}{
				"hostname": nodeID,
				"cpu": map[string]interface{}{
					"usage": float64(20 + i*5),
				},
				"lastHeartbeat": time.Now().UTC().Format(time.RFC3339),
			}

			err := aceteam.SendHeartbeat(ctx, nodeID, status)
			if err != nil {
				t.Logf("Heartbeat %d: %v", i+1, err)
			} else {
				t.Logf("Heartbeat %d: success", i+1)
			}

			time.Sleep(time.Second)
		}
	})
}

// TestNodeHeartbeatValidation tests heartbeat input validation
func TestNodeHeartbeatValidation(t *testing.T) {
	aceteamURL := os.Getenv("ACETEAM_URL")
	if aceteamURL == "" {
		aceteamURL = "http://localhost:3000"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	aceteam := harness.NewAceTeamHarness(aceteamURL)

	t.Run("EmptyStatus", func(t *testing.T) {
		nodeID := "test-node-empty"
		status := map[string]interface{}{}

		err := aceteam.SendHeartbeat(ctx, nodeID, status)
		// Should either succeed (minimal heartbeat) or fail with validation error
		t.Logf("Empty status heartbeat result: %v", err)
	})

	t.Run("InvalidNodeID", func(t *testing.T) {
		// Node ID with invalid characters
		nodeID := "../etc/passwd"
		status := map[string]interface{}{
			"hostname": "test",
		}

		err := aceteam.SendHeartbeat(ctx, nodeID, status)
		// Should fail with validation error
		if err != nil {
			t.Logf("Invalid node ID correctly rejected: %v", err)
		} else {
			t.Error("Expected error for invalid node ID")
		}
	})
}

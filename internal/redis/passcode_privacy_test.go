package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestDLQNeverRetainsTheNodePasscode(t *testing.T) {
	const pin = "synthetic-node-passcode-4821"
	for _, kind := range []string{"SHELL_COMMAND", "VNC_ACTIONS", "FILE_SCREENSHOT", "WORKFLOW_RUN"} {
		for _, multi := range []bool{false, true} {
			_, client, raw := setupMiniredis(t)
			ctx := context.Background()
			job := &Job{JobID: "fixture", Type: kind, Payload: map[string]any{"passcode": pin, "command": "true"}, RawData: map[string]any{"payload": pin}}
			var err error
			if multi {
				err = client.MoveToDLQFromQueue(ctx, "jobs:v1:integration-test", job, "failed: "+pin)
			} else {
				err = client.MoveToDLQ(ctx, job, "failed: "+pin)
			}
			if err != nil {
				t.Fatal(err)
			}
			entries, err := raw.XRange(ctx, "dlq:v1:integration-test", "-", "+").Result()
			if err != nil || len(entries) != 1 {
				t.Fatalf("%s: missing dead-letter entry", kind)
			}
			for key, field := range entries[0].Values {
				if text, ok := field.(string); ok && strings.Contains(text, pin) {
					t.Fatalf("%s: dead-letter field %q retained the passcode", kind, key)
				}
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(entries[0].Values["payload"].(string)), &payload); err != nil || payload["command"] != "true" {
				t.Fatalf("%s: DLQ payload lost its other fields: %v %v", kind, payload, err)
			}
			if job.Payload["passcode"] != pin {
				t.Fatalf("%s: DLQ retention must not mutate the in-flight payload", kind)
			}
		}
	}
}

func TestJobFormattingOmitsTheNodePasscode(t *testing.T) {
	const pin = "synthetic-node-passcode-4821"
	job := Job{JobID: "fixture", Type: "VNC_KEYS", Payload: map[string]any{"passcode": pin, "keys": "ctrl+c"}, RawPayload: []byte(pin), RawData: map[string]any{"payload": pin}}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	for _, rendered := range []string{string(encoded), fmt.Sprintf("%v", job), fmt.Sprintf("%+v", job)} {
		if strings.Contains(rendered, pin) {
			t.Fatalf("job rendering retained the passcode: %s", rendered)
		}
	}
	if !strings.Contains(string(encoded), "ctrl+c") {
		t.Fatalf("job rendering lost the other payload fields: %s", encoded)
	}
}

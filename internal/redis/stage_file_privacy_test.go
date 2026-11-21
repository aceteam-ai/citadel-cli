package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A COBROWSE_SESSION stage_file payload carries a user's file as base64. It
// never reaches a dead-letter entry or a rendering (citadel-cli#1721).
func TestCobrowseStagedFileBytesNeverRetained(t *testing.T) {
	const body = "U1lOVEhFVElDLUZJTEUtQllURVMtMTIzNDU2"
	_, client, raw := setupMiniredis(t)
	ctx := context.Background()
	job := &Job{JobID: "fixture", Type: "COBROWSE_SESSION", Payload: map[string]any{"bytes_b64": body, "action": "stage_file", "name": "filing.pdf"}, RawPayload: []byte(body), RawData: map[string]any{"payload": body}}
	if err := client.MoveToDLQ(ctx, job, "failed: "+body); err != nil {
		t.Fatal(err)
	}
	entries, err := raw.XRange(ctx, "dlq:v1:integration-test", "-", "+").Result()
	if err != nil || len(entries) != 1 {
		t.Fatal("missing dead-letter entry")
	}
	for key, field := range entries[0].Values {
		if text, ok := field.(string); ok && strings.Contains(text, body) {
			t.Fatalf("dead-letter field %q retained the file bytes", key)
		}
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(entries[0].Values["payload"].(string)), &payload); err != nil || payload["name"] != "filing.pdf" {
		t.Fatalf("DLQ payload lost its other fields: %v %v", payload, err)
	}
	encoded, _ := json.Marshal(*job)
	for _, rendered := range []string{string(encoded), fmt.Sprintf("%v", *job), fmt.Sprintf("%+v", *job)} {
		if strings.Contains(rendered, body) {
			t.Fatalf("job rendering retained the file bytes: %s", rendered)
		}
	}
}

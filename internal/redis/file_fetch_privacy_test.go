package redis

import (
	"context"
	"strings"
	"testing"
)

func TestFileFetchDLQRedactsBeforeHandler(t *testing.T) {
	for _, multi := range []bool{false, true} {
		_, client, raw := setupMiniredis(t)
		ctx := context.Background()
		const token = "synthetic-delivery.synthetic-credential"
		job := &Job{JobID: "fixture", Type: "FILE_FETCH", Payload: map[string]any{"capability": token}, RawData: map[string]any{"payload": token}}
		var err error
		if multi {
			err = client.MoveToDLQFromQueue(ctx, "jobs:v1:integration-test", job, "delivery failed: "+token)
		} else {
			err = client.MoveToDLQ(ctx, job, "delivery failed: "+token)
		}
		if err != nil {
			t.Fatal(err)
		}
		entries, err := raw.XRange(ctx, "dlq:v1:integration-test", "-", "+").Result()
		if err != nil || len(entries) != 1 {
			t.Fatal("missing sanitized dead-letter entry")
		}
		for _, field := range entries[0].Values {
			if text, ok := field.(string); ok && strings.Contains(text, token) {
				t.Fatal("dead-letter serialization retained a delivery credential")
			}
		}
		if entries[0].Values["payload"] != "null" || job.Payload["capability"] != token {
			t.Fatal("DLQ retention must omit payload without mutating in-flight authority")
		}
	}
}

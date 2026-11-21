package worker

import (
	"context"
	"testing"
	"time"
)

// A stage_file entry carries a user's file as base64; acknowledging it removes
// the entry from the stream. Other COBROWSE_SESSION actions keep normal
// retention.
func TestStageFileAckRemovesSourceEntry(t *testing.T) {
	raw, url := fileFetchDisposableRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	queue := "jobs:v1:privacy:node:stage"
	source := NewRedisSource(RedisSourceConfig{URL: url, QueueName: queue, ConsumerGroup: "workers", BlockMs: 1, LogFn: func(string, string) {}})
	if err := source.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer source.Close()

	for _, tc := range []struct {
		payload string
		deleted bool
	}{
		{`{"action":"stage_file","session_id":"cb-1","name":"a.pdf","bytes_b64":"QUJD"}`, true},
		{`{"action":"status","session_id":"cb-1"}`, false},
	} {
		id := enqueueTestJob(t, raw, queue, map[string]any{"jobId": "j", "type": JobTypeCobrowseSession, "payload": tc.payload})
		job, err := source.Next(ctx)
		if err != nil || job == nil {
			t.Fatalf("fixture: %v", err)
		}
		if err := source.Ack(ctx, job); err != nil {
			t.Fatal(err)
		}
		rows, err := raw.XRange(ctx, queue, id, id).Result()
		if err != nil {
			t.Fatal(err)
		}
		if tc.deleted && len(rows) != 0 {
			t.Fatalf("acked stage_file entry was retained: %s", tc.payload)
		}
		if !tc.deleted && len(rows) != 1 {
			t.Fatalf("a non-stage COBROWSE_SESSION entry must keep normal retention: %s", tc.payload)
		}
	}
}

package worker

import (
	"context"
	"testing"
)

// TestRedisSourceAddQueueCreatesStreamAndDelivers covers the worker side of the
// per-node routing fix (issue #3914): AddQueue must (1) create the per-node
// stream + consumer group immediately (so the platform dispatcher's
// consumer-presence check can see the stream), and (2) cause jobs enqueued to
// the per-node stream to be delivered to this worker.
//
// NOTE: the dispatcher gates routing on XINFO GROUPS reporting consumers > 0.
// miniredis does not track XREADGROUP consumers (XInfoGroups always reports 0),
// so that exact signal is verified against real Redis as a manual/integration
// test-plan item, not here. Real Redis registers the consumer on the first
// XREADGROUP, which the worker issues every poll once the queue is added.
func TestRedisSourceAddQueueCreatesStreamAndDelivers(t *testing.T) {
	_, source, raw := setupWorkerIntegration(t, "jobs:v1:shell:org_test", 3)
	ctx := context.Background()

	perNode := "jobs:v1:shell:org_test:node:1008"
	if err := source.AddQueue(ctx, perNode); err != nil {
		t.Fatalf("AddQueue failed: %v", err)
	}

	// The stream/group must exist immediately (EnsureConsumerGroups -> MKSTREAM)
	// so the dispatcher can inspect it before any job is written.
	groups, err := raw.XInfoGroups(ctx, perNode).Result()
	if err != nil {
		t.Fatalf("XInfoGroups after AddQueue failed (stream not created?): %v", err)
	}
	if len(groups) == 0 {
		t.Fatalf("expected a consumer group on %s after AddQueue", perNode)
	}

	// A job enqueued to the per-node stream is actually delivered to this worker.
	enqueueTestJob(t, raw, perNode, map[string]interface{}{
		"jobId":   "job-per-node",
		"type":    "SHELL_COMMAND",
		"payload": `{"command":"hostname","target_node":"1008"}`,
	})

	var got *Job
	for i := 0; i < 50; i++ {
		j, err := source.Next(ctx)
		if err != nil {
			t.Fatalf("Next() failed: %v", err)
		}
		if j != nil {
			got = j
			break
		}
	}
	if got == nil {
		t.Fatal("job enqueued to per-node stream was never delivered")
	}
	if got.ID != "job-per-node" {
		t.Errorf("delivered job ID = %q, want job-per-node", got.ID)
	}
	if got.SourceQueue != perNode {
		t.Errorf("SourceQueue = %q, want %q", got.SourceQueue, perNode)
	}
}

// TestRedisSourceAddQueueIdempotentAndBlank verifies AddQueue ignores blanks
// and duplicates so the per-node subscription wiring is safe to call defensively.
func TestRedisSourceAddQueueIdempotentAndBlank(t *testing.T) {
	_, source, _ := setupWorkerIntegration(t, "jobs:v1:shell:org_test", 3)
	ctx := context.Background()

	before := len(source.QueueNames())

	if err := source.AddQueue(ctx, ""); err != nil {
		t.Fatalf("AddQueue(\"\") returned error: %v", err)
	}
	if len(source.QueueNames()) != before {
		t.Errorf("blank queue must be ignored; queues changed to %v", source.QueueNames())
	}

	q := "jobs:v1:shell:org_test:node:42"
	if err := source.AddQueue(ctx, q); err != nil {
		t.Fatalf("AddQueue failed: %v", err)
	}
	if err := source.AddQueue(ctx, q); err != nil {
		t.Fatalf("second AddQueue failed: %v", err)
	}
	count := 0
	for _, name := range source.QueueNames() {
		if name == q {
			count++
		}
	}
	if count != 1 {
		t.Errorf("queue %q should appear exactly once, found %d times in %v", q, count, source.QueueNames())
	}
}

// TestAPISourceAddQueue verifies the API source appends queues, ignores blanks,
// and dedupes (group creation is handled lazily by the proxy on first read).
func TestAPISourceAddQueue(t *testing.T) {
	source := NewAPISource(APISourceConfig{
		QueueName:     "jobs:v1:shell:org_test",
		ConsumerGroup: "citadel-workers",
		LogFn:         func(level, msg string) {},
	})

	before := len(source.QueueNames())
	source.AddQueue("")
	if len(source.QueueNames()) != before {
		t.Errorf("blank queue must be ignored; queues = %v", source.QueueNames())
	}

	q := "jobs:v1:shell:org_test:node:1008"
	source.AddQueue(q)
	source.AddQueue(q)
	count := 0
	for _, name := range source.QueueNames() {
		if name == q {
			count++
		}
	}
	if count != 1 {
		t.Errorf("queue %q should appear exactly once, found %d in %v", q, count, source.QueueNames())
	}
}

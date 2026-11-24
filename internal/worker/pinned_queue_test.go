package worker

import (
	"context"
	"reflect"
	"sync"
	"testing"
)

func TestPinnedQueueSourcesCannotBeBroadened(t *testing.T) {
	const pin = "jobs:v1:shell:org_fixture:node:4321"
	api := NewAPISource(APISourceConfig{PinnedQueue: pin, QueueNames: []string{DefaultCPUQueue}, LogFn: func(string, string) {}})
	direct := NewRedisSource(RedisSourceConfig{PinnedQueue: pin, QueueNames: []string{DefaultCPUQueue}, LogFn: func(string, string) {}})
	for _, q := range []string{DefaultCPUQueue, "jobs:v1:gpu-general", "jobs:v1:shell:org_fixture", "jobs:v1:shell:org_fixture:node:other"} {
		api.AddQueue(q)
		if err := direct.AddQueue(context.Background(), q); err == nil {
			t.Fatalf("Redis accepted %s", q)
		}
	}
	// Even an incorrectly installed reconciler cannot add inference streams.
	r := NewInferenceQueueReconciler([]string{"jobs:v1:gpu-general"}, func(context.Context) bool { return true }, func(_ context.Context, q string) error { api.AddQueue(q); return nil })
	r.Reconcile(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			api.AddQueue(DefaultCPUQueue)
			api.AddQueue(pin)
			_ = direct.AddQueue(context.Background(), "jobs:v1:gpu-general")
			_ = direct.AddQueue(context.Background(), pin)
		}()
	}
	wg.Wait()
	for name, got := range map[string][]string{"api": api.QueueNames(), "redis": direct.QueueNames()} {
		if !reflect.DeepEqual(got, []string{pin}) {
			t.Fatalf("%s queues=%v", name, got)
		}
	}
}

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/capabilities"
	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"
)

func TestControlWorkerQueues(t *testing.T) {
	p := workerQueueParams{Role: config.WorkerRoleControl, NodeID: "1915", OrgID: "fixture-org", WorkQueue: "jobs:v1:assigned", Serving: true,
		NodeCaps: &capabilities.NodeCapabilities{GPU: &capabilities.GPUCapabilities{Devices: []capabilities.GPUDevice{{Name: "fixture GPU"}}}}}
	want := []string{nodeQueueName(p.OrgID, p.NodeID)}
	for _, held := range []bool{false, true} {
		p.WorkerHeld = held
		api := resolveWorkerQueues(context.Background(), p)
		direct := resolveDirectRedisQueuesForRole(p, []string{"meeting", "gpu:fixture"}, nil)
		for name, got := range map[string]workerQueueResult{"api": api, "direct": direct} {
			if got.Err != nil || !reflect.DeepEqual(got.Queues, want) || len(got.Missing) != 0 || got.WorkQueue != "" {
				t.Errorf("%s held=%t got %+v, want only %v and no inference reconciler", name, held, got, want)
			}
		}
	}
}

// A fixture-only config applier behind the real legacy dispatch adapter. The
// production config handler can manage VNC and must not run in this test.
type controlConfigFixture struct{ path string }

func (h controlConfigFixture) Execute(_ jobs.JobContext, job *nexus.Job) ([]byte, error) {
	if job.Type != worker.JobTypeApplyDeviceConfig {
		return nil, fmt.Errorf("not a config job")
	}
	var config struct {
		Name string `json:"fixture_name"`
	}
	if err := json.Unmarshal([]byte(job.Payload["config"]), &config); err != nil {
		return nil, err
	}
	return nil, os.WriteFile(h.path, []byte(config.Name), 0600)
}

func TestWorkerRoleConfigAndFlag(t *testing.T) {
	var dc DeviceConfig
	if err := yaml.Unmarshal([]byte("role: control\n"), &dc); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		dc   *DeviceConfig
		flag string
		want config.WorkerRole
		bad  bool
	}{
		{nil, "", config.WorkerRoleFull, false}, {&dc, "", config.WorkerRoleControl, false},
		{&dc, "full", config.WorkerRoleFull, false}, {nil, "control", config.WorkerRoleControl, false},
		{&DeviceConfig{Role: "typo"}, "", "", true}, {nil, "FULL", "", true},
	} {
		got, err := resolveWorkerRole(tt.dc, tt.flag)
		if (err != nil) != tt.bad || got != tt.want {
			t.Fatalf("role=%q err=%v want=%q bad=%v", got, err, tt.want, tt.bad)
		}
	}
	var out bytes.Buffer
	printConfiguredWorkerRole(&out, &dc)
	if !strings.Contains(out.String(), "control") || !strings.Contains(out.String(), "restart") {
		t.Fatal(out.String())
	}
	if flag := workCmd.Flags().Lookup("role"); flag == nil || flag.DefValue != "" {
		t.Fatal("role flag must inherit config by default")
	}
}

func TestFullWorkerQueueParity(t *testing.T) {
	p := workerQueueParams{OrgID: "fixture-org", WorkQueue: "jobs:v1:assigned", Serving: true}
	legacy := resolveWorkerQueues(context.Background(), p)
	p.Role = config.WorkerRoleFull
	if full := resolveWorkerQueues(context.Background(), p); !reflect.DeepEqual(full, legacy) {
		t.Fatalf("full changed legacy queues: %+v != %+v", full, legacy)
	}
	tags := []string{"gpu:fixture", "meeting"}
	if got := resolveDirectRedisQueuesForRole(p, tags, nil); !reflect.DeepEqual(got.Queues, resolveDirectRedisQueues(p.WorkQueue, tags, nil, p.OrgID)) {
		t.Fatal(got)
	}
}

// The broker and config handler are fakes; only the real resolver, source and
// Redis stream admission execute. No legacy native config side effects run.
func TestControlWorkerFakeBrokerKeepsSharedJobsAndAppliesNodeConfig(t *testing.T) {
	broker := miniredis.RunT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client := redis.NewClient(&redis.Options{Addr: broker.Addr()})
	defer client.Close()
	p := workerQueueParams{Role: config.WorkerRoleControl, OrgID: "fixture-org", NodeID: "1915", WorkQueue: worker.DefaultCPUQueue}
	resolved := resolveDirectRedisQueuesForRole(p, nil, nil)
	if resolved.Err != nil {
		t.Fatal(resolved.Err)
	}
	source := worker.NewRedisSource(worker.RedisSourceConfig{URL: "redis://" + broker.Addr(), QueueNames: resolved.Queues,
		PinnedQueue: controlPinnedQueue(p.Role, resolved.Queues), ConsumerGroup: "fixture-workers", BlockMs: 1, LogFn: func(string, string) {}})
	if err := source.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	for _, queue := range []string{worker.DefaultCPUQueue, shellQueueName(p.OrgID), resolved.Queues[0]} {
		if err := client.XAdd(ctx, &redis.XAddArgs{Stream: queue, Values: map[string]any{
			"jobId": queue, "type": worker.JobTypeApplyDeviceConfig, "payload": `{"config":"{\"fixture_name\":\"configured\"}"}`,
		}}).Err(); err != nil {
			t.Fatal(err)
		}
	}
	// A later resubscribe or inference transition cannot broaden the source.
	for _, queue := range []string{worker.DefaultCPUQueue, shellQueueName(p.OrgID), "jobs:v1:gpu-general"} {
		if err := source.AddQueue(ctx, queue); err == nil {
			t.Fatalf("accepted shared queue %s", queue)
		}
	}
	job, err := source.Next(ctx)
	if err != nil || job == nil || job.SourceQueue != resolved.Queues[0] {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	path := filepath.Join(t.TempDir(), "applied-config")
	handler := worker.NewLegacyHandlerAdapter(worker.JobTypeApplyDeviceConfig, controlConfigFixture{path: path})
	if !handler.CanHandle(job.Type) {
		t.Fatalf("config job not dispatchable: %s", job.Type)
	}
	if _, err := handler.Execute(ctx, job, &worker.NoOpStreamWriter{}); err != nil {
		t.Fatal(err)
	}
	if applied, err := os.ReadFile(path); err != nil || string(applied) != "configured" {
		t.Fatalf("config not applied: %s %v", applied, err)
	}
	if err := source.Ack(ctx, job); err != nil {
		t.Fatal(err)
	}
	if job, err = source.Next(ctx); err != nil || job != nil {
		t.Fatalf("shared job claimed: %+v %v", job, err)
	}
	for _, queue := range []string{worker.DefaultCPUQueue, shellQueueName(p.OrgID)} {
		groups, err := client.XInfoGroups(ctx, queue).Result()
		if err != nil || len(groups) != 0 {
			t.Fatalf("shared stream was touched: %s groups=%v err=%v", queue, groups, err)
		}
		if n := client.XLen(ctx, queue).Val(); n != 1 {
			t.Fatalf("shared job removed from %s", queue)
		}
	}
}

func TestControlWorkerMissingIdentityRefuses(t *testing.T) {
	for _, p := range []workerQueueParams{{Role: config.WorkerRoleControl, OrgID: "org"}, {Role: config.WorkerRoleControl, NodeID: "1915"}} {
		for _, got := range []workerQueueResult{resolveWorkerQueues(context.Background(), p), resolveDirectRedisQueuesForRole(p, nil, nil)} {
			if got.Err == nil || len(got.Queues) != 0 {
				t.Fatalf("missing identity must refuse without fallback: %+v", got)
			}
		}
	}
}

func TestControlWorkerIdentityPreflight(t *testing.T) {
	calls := 0
	lookup := func(context.Context) string { calls++; return "1915" }
	pause := func(context.Context, time.Duration) bool { return false }
	if _, err := controlWorkerIdentityFrom(context.Background(), "", lookup, pause); err == nil || calls != 0 {
		t.Fatal("missing org must refuse before network lookup")
	}
	if id, err := controlWorkerIdentityFrom(context.Background(), "fixture-org", lookup, pause); err != nil || id != "1915" {
		t.Fatalf("id=%s err=%v", id, err)
	}
	if id, err := controlWorkerIdentityFrom(context.Background(), "fixture-org", func(context.Context) string { return "" }, pause); err == nil || id != "" {
		t.Fatal("missing node must refuse")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls = 0
	if _, err := controlWorkerIdentityFrom(ctx, "fixture-org", lookup, pause); err == nil || calls != 0 {
		t.Fatal("cancelled lookup must not select a source")
	}
}

type roleFixtureTransport func(*http.Request) (*http.Response, error)

func (f roleFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWorkerRoleActiveProbe(t *testing.T) {
	for _, tt := range []struct {
		body  string
		want  config.WorkerRole
		known bool
	}{
		{`{"role":"control","worker":{"consuming":true}}`, config.WorkerRoleControl, true},
		{`{"worker":{"consuming":true}}`, config.WorkerRoleFull, true},
		{`{"role":"unknown","worker":{}}`, "", false},
		{`{"role":7,"worker":{}}`, "", false},
		{`{"role":"control"}`, "", false},
		{`broken`, "", false},
	} {
		client := &http.Client{Transport: roleFixtureTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() != "http://127.0.0.1:1/worker" {
				t.Fatal("unexpected probe target")
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tt.body))}, nil
		})}
		got, known := probeActiveWorkerRoleAt(client, "http://127.0.0.1:1/worker")
		if got != tt.want || known != tt.known {
			t.Fatalf("body=%s got=%q known=%v", tt.body, got, known)
		}
	}
}

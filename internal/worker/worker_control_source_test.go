package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
)

func controlMetadata() *redisapi.WorkerConfigResponse {
	return &redisapi.WorkerConfigResponse{OrgID: "example", WorkerControl: &redisapi.WorkerControlConfig{
		Protocol: WorkerControlProtocol, Queue: workerControlQueue("example", "758"), Group: WorkerControlProtocol,
	}}
}

func TestWorkerControlMetadataRequiresMatchingLocalIdentity(t *testing.T) {
	cases := []struct {
		name string
		edit func(**redisapi.WorkerConfigResponse, *string, *string, *string)
	}{
		{"missing config", func(c **redisapi.WorkerConfigResponse, _, _, _ *string) { *c = nil }},
		{"legacy metadata absent", func(c **redisapi.WorkerConfigResponse, _, _, _ *string) { (*c).WorkerControl = nil }},
		{"missing token", func(_ **redisapi.WorkerConfigResponse, token, _, _ *string) { *token = "" }},
		{"missing local org", func(_ **redisapi.WorkerConfigResponse, _, org, _ *string) { *org = "" }},
		{"missing local node", func(_ **redisapi.WorkerConfigResponse, _, _, node *string) { *node = "" }},
		{"wrong org", func(_ **redisapi.WorkerConfigResponse, _, org, _ *string) { *org = "other" }},
		{"wrong node", func(_ **redisapi.WorkerConfigResponse, _, _, node *string) { *node = "759" }},
		{"wrong protocol", func(c **redisapi.WorkerConfigResponse, _, _, _ *string) {
			(*c).WorkerControl.Protocol = "worker-control-v2"
		}},
		{"wrong group", func(c **redisapi.WorkerConfigResponse, _, _, _ *string) { (*c).WorkerControl.Group = "citadel-workers" }},
		{"old shell queue", func(c **redisapi.WorkerConfigResponse, _, _, _ *string) {
			(*c).WorkerControl.Queue = "jobs:v1:shell:org_example:node:758"
		}},
		{"wrong queue org", func(c **redisapi.WorkerConfigResponse, _, _, _ *string) {
			(*c).WorkerControl.Queue = workerControlQueue("other", "758")
		}},
		{"wrong queue node", func(c **redisapi.WorkerConfigResponse, _, _, _ *string) {
			(*c).WorkerControl.Queue = workerControlQueue("example", "759")
		}},
	}
	if !validateWorkerControl(controlMetadata(), "test-device-token", "example", "758") {
		t.Fatal("matching server metadata should opt in")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, token, org, node := controlMetadata(), "test-device-token", "example", "758"
			tc.edit(&cfg, &token, &org, &node)
			if validateWorkerControl(cfg, token, org, node) {
				t.Fatal("unsafe opt-in")
			}
		})
	}
}

func TestWorkerControlRefusesUnauthenticatedDeliveryAndResults(t *testing.T) {
	cases := []struct {
		name   string
		edit   func(*Job)
		writer func(*Job) StreamWriter
	}{
		{"direct redis", func(j *Job) { j.Source = "redis" }, nil},
		{"legacy api", func(j *Job) { j.control = nil }, nil},
		{"wrong queue", func(j *Job) { j.SourceQueue = workerControlQueue("example", "759") }, nil},
		{"wrong org", func(j *Job) { j.SourceQueue = workerControlQueue("other", "758") }, nil},
		{"old shell queue", func(j *Job) { j.SourceQueue = "jobs:v1:shell:org_example:node:758" }, nil},
		{"wrong group", func(j *Job) { j.control.group = "citadel-workers" }, nil},
		{"missing job id", func(j *Job) { j.ID = "" }, nil},
		{"no authenticated result path", nil, func(*Job) StreamWriter { return &NoOpStreamWriter{} }},
		{"wrong result client", nil, func(j *Job) StreamWriter {
			return NewAPIStreamWriter(context.Background(), redisapi.NewClient(redisapi.ClientConfig{}), j.ID, "")
		}},
		{"wrong result id", nil, func(j *Job) StreamWriter {
			return NewAPIStreamWriter(context.Background(), j.control.client, "other-job", "")
		}},
		{"direct redis result", nil, func(j *Job) StreamWriter { return NewRedisStreamWriter(context.Background(), nil, j.ID, "") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := controlJob("negative")
			if tc.edit != nil {
				tc.edit(job)
			}
			events := []string{}
			writer := StreamWriter(&controlWriter{events: &events})
			if tc.writer != nil {
				writer = tc.writer(job)
			}
			h := NewWorkerControlHandler(WorkerControlConfig{OrgID: "example", NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { t.Fatal("reached service management"); return true }})
			source := &controlSource{MockJobSource: NewMockJobSource("redis-api", nil), events: &events}
			runner := NewRunner(source, []JobHandler{h}, RunnerConfig{})
			runner.WithStreamWriterFactory(func(*Job) StreamWriter { return writer })
			if proceed, _, _ := runner.claimJob(context.Background(), job); proceed {
				t.Fatal("claimed unsafe control")
			}
			if runner.executeJob(context.Background(), job, writer, time.Now(), false, 0) {
				t.Fatal("executed unsafe control")
			}
			if len(events) != 0 {
				t.Fatalf("published or acknowledged unsafe delivery: %v", events)
			}
		})
	}
}

func TestWorkerControlQueueCannotBeSubscribedByLegacySources(t *testing.T) {
	ctx := context.Background()
	queue := workerControlQueue("example", "758")
	for _, group := range []string{"citadel-workers", WorkerControlProtocol} {
		api := NewAPISource(APISourceConfig{QueueName: queue, ConsumerGroup: group, Token: "test-token"})
		if err := api.Connect(ctx); err == nil {
			t.Fatal("generic API source accepted control queue")
		}
		direct := NewRedisSource(RedisSourceConfig{QueueName: queue, ConsumerGroup: group})
		if err := direct.Connect(ctx); err == nil {
			t.Fatal("direct Redis accepted control queue")
		}
		if err := direct.AddQueue(ctx, queue); err == nil {
			t.Fatal("direct Redis resubscription accepted control queue")
		}
	}
	api := NewAPISource(APISourceConfig{QueueName: DefaultCPUQueue})
	api.AddQueue(queue)
	if _, err := api.readOnce(ctx, 1); err == nil {
		t.Fatal("generic API resubscription consumed control queue")
	}
}

func TestWorkerControlAuthenticatedRESTContractAndOutcomeOrder(t *testing.T) {
	const jobID = "55d962d8-52a0-4910-b40b-534a14a6f417"
	var mu sync.Mutex
	events := []string{}
	record := func(event string) { mu.Lock(); events = append(events, event); mu.Unlock() }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-device-token" {
			t.Error("missing device authentication")
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/fabric/redis/ping":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/api/fabric/worker-config":
			_ = json.NewEncoder(w).Encode(controlMetadata())
		case "/api/fabric/redis/jobs/consume":
			var req redisapi.ConsumeRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			if req.Queue != workerControlQueue("example", "758") || req.Group != WorkerControlProtocol {
				t.Errorf("consume=%+v", req)
			}
			_ = json.NewEncoder(w).Encode(redisapi.ConsumeResponse{Messages: []redisapi.StreamMessage{{ID: "1-0", Data: redisapi.StreamMessageData{JobID: jobID, Type: JobTypeWorkerControl, Payload: `{"action":"restart","target_node":"758"}`}}}})
		case "/api/fabric/redis/jobs/acknowledge":
			var req redisapi.AcknowledgeRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Queue != workerControlQueue("example", "758") || req.Group != WorkerControlProtocol || req.MessageID != "1-0" {
				t.Errorf("ack=%+v", req)
			}
			record("ack")
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
		case "/api/fabric/redis/pubsub/publish":
			var req redisapi.PublishRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			var event map[string]any
			if err := json.Unmarshal([]byte(req.Message), &event); err != nil {
				t.Error(err)
			}
			if req.Channel != "stream:v1:"+jobID || event["jobId"] != jobID {
				t.Errorf("wrong exact ID: %+v %+v", req, event)
			}
			for _, key := range []string{"signature", "worker_control_signature", "control_signature"} {
				if _, ok := event[key]; ok {
					t.Errorf("fabricated server field %s", key)
				}
			}
			record(event["type"].(string))
			_, _ = w.Write([]byte(`{"published":true}`))
		default:
			t.Errorf("unexpected control side channel: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	api := NewAPISource(APISourceConfig{BaseURL: srv.URL, Token: "test-device-token", QueueName: DefaultCPUQueue, ConsumerGroup: "normal-jobs"})
	if err := api.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	control, err := api.WorkerControlSource(ctx, "example", "758")
	if err != nil || control == nil {
		t.Fatalf("source=%v err=%v", control, err)
	}
	if !reflect.DeepEqual(api.QueueNames(), []string{DefaultCPUQueue}) {
		t.Fatal("control leaked into normal queue list")
	}
	if err := control.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	job, err := control.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := NewWorkerControlHandler(WorkerControlConfig{OrgID: "example", NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Schedule: func() (func(), func(), error) {
		record("schedule")
		return func() { record("restart") }, func() {}, nil
	}})
	runner := NewRunner(control, []JobHandler{h}, RunnerConfig{NodeID: "758"})
	factory := CreateAPIStreamWriterFactory(ctx, control)
	runner.WithStreamWriterFactory(factory)
	writer := factory(job)
	if err := writer.WriteEnd(map[string]any{"accepted": true}); err == nil {
		t.Fatal("terminal before claim accepted")
	}
	proceed, stream, start := runner.claimJob(ctx, job)
	if !proceed || !runner.executeJob(ctx, job, stream, start, false, 0) {
		t.Fatal("authenticated restart failed")
	}
	if err := stream.WriteClaimed("late"); err == nil {
		t.Fatal("late claim reopened terminal")
	}
	if err := stream.WriteError(errors.New("late error"), false); err == nil {
		t.Fatal("late error replaced terminal")
	}
	if err := stream.WriteEnd(map[string]any{"accepted": false}); err == nil {
		t.Fatal("conflicting duplicate end replaced terminal")
	}
	mu.Lock()
	got := append([]string(nil), events...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []string{"claimed", "ack", "schedule", "end", "restart"}) {
		t.Fatalf("ordering=%v", got)
	}
}

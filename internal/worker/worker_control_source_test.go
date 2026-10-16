package worker

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	"github.com/aceteam-ai/citadel-cli/internal/workercontrol"
	"github.com/aceteam-ai/citadel-cli/internal/workercontrol/controltest"
)

func controlMetadata() *redisapi.WorkerConfigResponse {
	c := redisapi.WorkerControlConfig(controltest.Load().Config())
	return &redisapi.WorkerConfigResponse{OrgID: fixtureOrg, WorkerControl: &c}
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
			(*c).WorkerControl.Protocol = "worker-control-v1"
		}},
		{"wrong group", func(c **redisapi.WorkerConfigResponse, _, _, _ *string) { (*c).WorkerControl.Group = "citadel-workers" }},
		{"old shell queue", func(c **redisapi.WorkerConfigResponse, _, _, _ *string) {
			(*c).WorkerControl.Queue = "jobs:v1:shell:org_example:node:758"
		}},
		{"wrong queue org", func(c **redisapi.WorkerConfigResponse, _, _, _ *string) {
			(*c).WorkerControl.Queue = workerControlQueue("other", "758")
		}},
		{"wrong queue node", func(c **redisapi.WorkerConfigResponse, _, _, _ *string) {
			(*c).WorkerControl.Queue = workerControlQueue(fixtureOrg, "759")
		}},
	}
	if !validateWorkerControl(controlMetadata(), "test-device-token", fixtureOrg, "758") {
		t.Fatal("matching complete authenticated metadata must enable the v2 source")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, token, org, node := controlMetadata(), "test-device-token", fixtureOrg, "758"
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
		{"wrong queue", func(j *Job) { j.SourceQueue = workerControlQueue(fixtureOrg, "759") }, nil},
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
			h := NewWorkerControlHandler(WorkerControlConfig{OrgID: fixtureOrg, NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { t.Fatal("reached service management"); return true }})
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
	queue := workerControlQueue(fixtureOrg, "758")
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

func TestWorkerControlV1OptOutPreservesOrdinaryJobsAndHealth(t *testing.T) {
	const ordinaryJob = "ordinary-job"
	var mu sync.Mutex
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		calls[r.URL.Path]++
		mu.Unlock()
		switch r.URL.Path {
		case "/api/fabric/redis/ping":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/api/fabric/worker-config":
			metadata := controlMetadata()
			metadata.WorkerControl.Protocol = "worker-control-v1"
			_ = json.NewEncoder(w).Encode(metadata)
		case "/api/fabric/redis/jobs/consume":
			var request redisapi.ConsumeRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			if request.Queue != DefaultCPUQueue || request.Group != "ordinary-workers" {
				t.Errorf("unsafe consumption: %+v", request)
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(redisapi.ConsumeResponse{Messages: []redisapi.StreamMessage{{ID: "1-0", Data: redisapi.StreamMessageData{JobID: ordinaryJob, Type: "SHELL_COMMAND", Payload: `{"command":"true"}`}}}})
		default:
			t.Errorf("unexpected side channel: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	api := NewAPISource(APISourceConfig{BaseURL: srv.URL, Token: "synthetic-token", QueueName: DefaultCPUQueue, ConsumerGroup: "ordinary-workers"})
	if err := api.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	control, err := api.WorkerControlSource(ctx, fixtureOrg, "758")
	if err != nil || control != nil {
		t.Fatalf("v1 control source=%v err=%v", control, err)
	}
	job, err := api.Next(ctx)
	if err != nil || job == nil || job.ID != ordinaryJob {
		t.Fatalf("ordinary consumption broken: job=%v err=%v", job, err)
	}
	if err := api.Client().Ping(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["/api/fabric/redis/ping"] < 2 || calls["/api/fabric/worker-config"] != 1 {
		t.Fatalf("ordinary health/config calls=%v", calls)
	}
}

func TestWorkerControlAuthenticatedRESTContractAndOutcomeOrder(t *testing.T) {
	f := controltest.Load()
	f.Identity.AttemptID = uuid.NewString()
	f.Identity.ExpiresAt = time.Now().Add(time.Minute).UnixMilli()
	var mu sync.Mutex
	events := []string{}
	record := func(s string) { mu.Lock(); events = append(events, s); mu.Unlock() }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Error("missing device authentication")
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/api/fabric/redis/ping":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/api/fabric/worker-config":
			c := redisapi.WorkerControlConfig(f.Config())
			_ = json.NewEncoder(w).Encode(redisapi.WorkerConfigResponse{OrgID: fixtureOrg, WorkerControl: &c})
		case "/api/fabric/redis/jobs/consume":
			var req redisapi.ConsumeRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Queue != f.Identity.Queue || req.Group != WorkerControlProtocol {
				t.Error("wrong queue/group")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{map[string]any{"id": f.Deliveries["A"].MessageID, "data": f.Data()}}})
		case "/api/fabric/worker-control/operation":
			raw, _ := io.ReadAll(r.Body)
			var req workercontrol.Request
			if workercontrol.Decode(raw, &req, 16384) != nil || req.Validate() != nil {
				t.Error("invalid typed request")
				w.WriteHeader(400)
				return
			}
			record(req.Operation)
			_ = json.NewEncoder(w).Encode(controltest.Accept(req, time.Now().UnixMilli()))
		default:
			t.Errorf("generic route used for control: %s", r.URL.Path)
			w.WriteHeader(403)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	api := NewAPISource(APISourceConfig{BaseURL: srv.URL, Token: "synthetic-token", QueueName: DefaultCPUQueue})
	if err := api.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	control, err := api.WorkerControlSource(ctx, fixtureOrg, "758")
	if err != nil || control == nil {
		t.Fatalf("control=%v err=%v", control, err)
	}
	if err := control.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	job, err := control.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := NewWorkerControlHandler(WorkerControlConfig{OrgID: fixtureOrg, NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Schedule: func(valid func() bool) (func(), func(), error) {
		record("schedule")
		return func() {
			if !valid() {
				t.Error("invalid permit")
			}
			record("restart")
		}, func() { record("cancel") }, nil
	}})
	runner := NewRunner(control, []JobHandler{h}, RunnerConfig{NodeID: "758"})
	factory := CreateAPIStreamWriterFactory(ctx, control)
	runner.WithStreamWriterFactory(factory)
	if factory(job).WriteEnd(map[string]any{"accepted": true}) == nil {
		t.Fatal("completion before claim")
	}
	// The invalid completion irreversibly cancelled that pipeline. A replacement
	// source must return the same cancelled Session, rather than reconstruct it.
	if factory(job).WriteClaimed("cancelled") == nil {
		t.Fatal("cancelled pipeline reopened")
	}
	// A distinct durable attempt exercises the positive transaction ordering.
	f.Identity.AttemptID = uuid.NewString()
	job, err = control.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	proceed, stream, start := runner.claimJob(ctx, job)
	if !proceed || !runner.executeJob(ctx, job, stream, start, false, 0) {
		t.Fatal("confirmed restart failed")
	}
	if stream.WriteClaimed("late") == nil {
		t.Fatal("late claim reopened terminal")
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(events, []string{"claim_and_ack", "schedule", "complete", "restart"}) {
		t.Fatalf("ordering=%v", events)
	}
}

func TestWorkerControlWSDeliveryAdapterPreservesTypedEnvelope(t *testing.T) {
	f := controltest.Load()
	f.Identity.AttemptID = uuid.NewString()
	f.Identity.ExpiresAt = time.Now().Add(time.Minute).UnixMilli()
	data := f.Data()
	raw, _ := json.Marshal(data)
	var fields map[string]string
	_ = json.Unmarshal(raw, &fields)
	client := redisapi.NewClient(redisapi.ClientConfig{Token: "synthetic-token"})
	s := NewWSSource(WSSourceConfig{Client: client, QueueName: f.Identity.Queue, ConsumerGroup: WorkerControlProtocol})
	s.control = &workerControlBinding{orgID: fixtureOrg, nodeID: "758", queue: f.Identity.Queue, group: WorkerControlProtocol, client: client, config: f.Config()}
	msg := redisapi.WSMessage{Type: "job", Queue: f.Identity.Queue, ID: f.Deliveries["A"].MessageID, Data: fields}
	job, err := s.convertWSJob(msg)
	if err != nil || job.controlEnvelope == nil || job.controlEnvelope.Identity != f.Identity {
		t.Fatalf("typed WS mapping err=%v", err)
	}
	msg.Data["extra"] = "forbidden"
	if _, err := s.convertWSJob(msg); err == nil {
		t.Fatal("unknown control data field accepted")
	}
}

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

func TestDurableRealAdapterWarmingLapsesOnlyStructuredMediaReply(t *testing.T) {
	for _, tc := range []struct {
		name, typ, reply string
		lapse            bool
	}{
		{"media-warming", JobTypeMediaGenerate, `{"status":"model_warming","retry_after_ms":1000}`, true},
		{"media-ready", JobTypeMediaGenerate, `{"status":"success","path":"fixture"}`, false},
		{"shell-stdout-is-data", JobTypeShellCommand, `{"status":"model_warming"}`, false},
		{"not-a-single-reply", JobTypeMediaGenerate, `{"status":"model_warming"} {}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeliveryFixture(t, tc.typ, `{}`)
			adapter := NewLegacyHandlerAdapter(tc.typ, deliveryCooperativeLegacy{run: func(jobs.JobContext) ([]byte, error) { return []byte(tc.reply), nil }})
			r, w := f.runner(adapter)
			proceed, stream, start := r.claimJob(context.Background(), f.job)
			if !proceed {
				t.Fatal("fixture not claimed")
			}
			ok := r.executeJob(context.Background(), f.job, stream, start, false, 0)
			if tc.lapse {
				if ok || len(f.envelopes) != 0 || w.ends+w.errs+w.cancels != 0 || f.job.delivery().valid() {
					t.Fatal("warming became a terminal", f.order)
				}
			} else if !ok || len(f.envelopes) != 1 || w.ends != 1 {
				t.Fatal("ordinary handler output lost", f.order)
			}
			f.check(t)
		})
	}
}

func TestDurableWorkerStopPreservesFinishedSuccessButLapsesUnfinished(t *testing.T) {
	for _, tc := range []struct {
		name                                    string
		success, inheritedDeadline, jobDeadline bool
	}{
		{name: "stopped-error"},
		{name: "finished-success", success: true},
		{name: "worker-deadline", inheritedDeadline: true},
		{name: "job-watchdog-before-stop", success: true, jobDeadline: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newDeliveryFixture(t, "CLOCK_FIXTURE", `{"timeout_ms":1000}`)
				root, cancel := context.WithCancel(context.Background())
				if tc.inheritedDeadline {
					cancel()
					root, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
				}
				defer cancel()
				h := &clockFixtureHandler{run: func(ctx context.Context, _ *Job, _ StreamWriter) (*JobResult, error) {
					if tc.jobDeadline {
						time.Sleep(1100 * time.Millisecond)
					}
					if tc.inheritedDeadline {
						<-ctx.Done()
					} else {
						cancel()
					}
					if tc.success {
						return &JobResult{Status: JobStatusSuccess, Output: map[string]any{"content": "finished"}}, nil
					}
					<-ctx.Done()
					return nil, ctx.Err()
				}}
				r, w := f.runner(h)
				proceed, stream, start := r.claimJob(root, f.job)
				if !proceed {
					t.Fatal("fixture not claimed")
				}
				ok := r.executeJob(root, f.job, stream, start, false, 0)
				switch {
				case tc.jobDeadline:
					if ok || len(f.envelopes) != 1 || w.errs != 1 || w.ends+w.cancels != 0 {
						t.Fatal("job watchdog masked by worker stop", f.order)
					}
				case tc.success:
					if !ok || len(f.envelopes) != 1 || w.ends != 1 || w.cancels != 0 {
						t.Fatal("finished result replaced by worker cancellation", f.order)
					}
					var body map[string]map[string]any
					if json.Unmarshal(f.envelopes[0], &body) != nil || body["result"]["content"] != "finished" {
						t.Fatal("finished content lost")
					}
				default:
					if ok || len(f.envelopes) != 0 || w.ends+w.errs+w.cancels != 0 || f.job.delivery().valid() {
						t.Fatal("worker shutdown became a job terminal", f.order)
					}
				}
				f.check(t)
			})
		})
	}
}

func TestDurableShutdownPausesRenewalWhileRetainingActualExitCustody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryFixture(t, "CLOCK_FIXTURE", `{"timeout_ms":10000}`, func(f *deliveryFixture) { f.budget = 1000 })
		started, stopped, exit := make(chan struct{}), make(chan struct{}), make(chan struct{})
		h := &clockFixtureHandler{run: func(ctx context.Context, _ *Job, _ StreamWriter) (*JobResult, error) {
			close(started)
			<-ctx.Done()
			close(stopped)
			<-exit
			return nil, ctx.Err()
		}}
		r, w := f.runner(h)
		root, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, stream, start := r.claimJob(root, f.job)
		done := make(chan bool, 1)
		lane := r.unboundedLane
		go func() { done <- r.runLaneJob(root, lane, f.job, stream, start, lane.reserveExecTicket()) }()
		released, drained := false, false
		defer func() {
			if !released {
				close(exit)
			}
			if !drained {
				<-done
			}
		}()
		<-started
		cancel()
		<-stopped
		synctest.Wait()
		before := f.renewalCount()
		time.Sleep(600 * time.Millisecond)
		if f.renewalCount() != before {
			t.Fatal("worker shutdown kept extending its claim")
		}
		select {
		case <-done:
			drained = true
			t.Fatal("worker shutdown released custody before handler exit")
		default:
		}
		if len(lane.exec) != 1 || lane.snapshot().Executing != 1 {
			t.Fatal("actual-exit bracket released")
		}
		close(exit)
		released = true
		ok := <-done
		drained = true
		if ok || len(f.envelopes) != 0 || w.ends+w.errs+w.cancels != 0 {
			t.Fatal("unfinished worker stop published terminal", f.order)
		}
		if len(lane.exec) != 0 || lane.snapshot().Executing != 0 {
			t.Fatal("exited handler retained lane")
		}
		f.check(t)
	})
}

func TestDurableCompletionPausesRenewalBeforeRequestWithoutLosingHeldLease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int64
		started, ended := make(chan struct{}), make(chan struct{})
		var d *durableDelivery
		c := fakeDeliveryControl{renew: func(ctx context.Context) (time.Duration, error) {
			if calls.Add(1) == 1 {
				return time.Second, nil
			}
			close(started)
			<-ctx.Done()
			close(ended)
			return 0, ctx.Err()
		}, complete: func(ctx context.Context, _ []byte) error {
			if d.loop.Err() == nil {
				return errors.New("renewal loop still live at completion")
			}
			<-ended // cancellation must not revoke the original lease before the receipt
			synctest.Wait()
			if !d.valid() || ctx.Err() != nil {
				return errors.New("paused renewal revoked held lease")
			}
			return nil
		}}
		var err error
		d, err = startDurableDelivery(context.Background(), c, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer d.stop()
		<-started
		f := &deliveryFixture{}
		w := &deliveryWriter{f: f}
		d.raw = w
		if err = d.stage(deliveryTerminal{kind: "result", output: json.RawMessage(`{"finished":true}`)}); err != nil {
			t.Fatal(err)
		}
		if err = d.finish(context.Background(), nil, ""); err != nil || w.ends != 1 || !d.isCommitted() {
			t.Fatal("overlapping renewal lost completed publisher", err)
		}
	})
}

func TestDurableTargetMismatchLapsesAndUnresolvedCommits(t *testing.T) {
	for _, own := range []string{"123", ""} {
		t.Run("node-"+own, func(t *testing.T) {
			f := newDeliveryFixture(t, "CLOCK_FIXTURE", `{"target_node":"456"}`)
			var executed atomic.Bool
			r, w := f.runner(&clockFixtureHandler{run: func(context.Context, *Job, StreamWriter) (*JobResult, error) { executed.Store(true); return nil, nil }})
			r.config.NodeID = own
			proceed, _, _ := r.claimJob(context.Background(), f.job)
			if proceed || executed.Load() {
				t.Fatal("wrong target was admitted")
			}
			if own != "" {
				if len(f.envelopes) != 0 || w.ends+w.errs+w.cancels != 0 || f.job.delivery().valid() {
					t.Fatal("target mismatch became permanent")
				}
			} else {
				if len(f.envelopes) != 1 || w.errs != 1 {
					t.Fatal("unresolved target lacks terminal")
				}
				var body map[string]map[string]any
				_ = json.Unmarshal(f.envelopes[0], &body)
				if body["error"]["code"] != "target_unresolved" {
					t.Fatal("wrong unresolved code")
				}
			}
			f.check(t)
		})
	}
}

func TestDurableRefusedRedisPendingDoesNotStarveFreshLegacy(t *testing.T) {
	for _, multi := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "multi"}[multi], func(t *testing.T) {
			server := miniredis.RunT(t)
			ctx := context.Background()
			queue := "jobs:v1:tag:fixture"
			source := NewRedisSource(RedisSourceConfig{URL: "redis://" + server.Addr(), QueueName: queue, ConsumerGroup: "fixture", MaxAttempts: 2, LogFn: func(string, string) {}})
			if err := source.Connect(ctx); err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			source.client.SetStalePendingReclaimMinIdle(time.Millisecond)
			raw := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
			defer raw.Close()
			id, err := raw.XAdd(ctx, &goredis.XAddArgs{Stream: queue, Values: map[string]any{"jobId": "unsupported", "type": "CLOCK_FIXTURE", "payload": "malformed-json", "Protocol": ""}}).Result()
			if err != nil {
				t.Fatal(err)
			}
			next := func() (*Job, error) {
				if multi {
					return source.nextMulti(ctx, []string{queue}, -1)
				}
				return source.nextSingle(ctx, queue, -1)
			}
			if job, err := next(); err != nil || job != nil || source.RefusedDeliveries() != 1 {
				t.Fatal("malformed v1 refusal became a fetch error", err)
			}
			// Use explicit idle time rather than sleeping on a real clock.
			if err = raw.Do(ctx, "XCLAIM", queue, "fixture", source.client.WorkerID(), "0", id, "IDLE", "3600000").Err(); err != nil {
				t.Fatal(err)
			}
			if err = raw.XAdd(ctx, &goredis.XAddArgs{Stream: queue, Values: map[string]any{"jobId": "legacy", "type": "CLOCK_FIXTURE", "payload": "{}"}}).Err(); err != nil {
				t.Fatal(err)
			}
			job, err := next()
			if err != nil || job == nil || job.ID != "legacy" || source.RefusedDeliveries() != 2 {
				t.Fatal("refused PEL entry starved fresh legacy delivery", err, source.RefusedDeliveries())
			}
			pending, err := raw.XPending(ctx, queue, "fixture").Result()
			if err != nil || pending.Count != 2 {
				t.Fatal("refusal was ACKed", pending, err)
			}
			keys := server.Keys()
			for _, key := range keys {
				if key == queue+":dlq" {
					t.Fatal("refusal dead-lettered")
				}
			}
		})
	}
}

func TestDurableQueuedJobMarkerCommitsCancellationWithoutExecution(t *testing.T) {
	f := newDeliveryFixture(t, "CLOCK_FIXTURE", `{}`)
	var executed atomic.Bool
	r, w := f.runner(&clockFixtureHandler{run: func(context.Context, *Job, StreamWriter) (*JobResult, error) { executed.Store(true); return nil, nil }})
	proceed, stream, start := r.claimJob(context.Background(), f.job)
	if !proceed {
		t.Fatal("not claimed")
	}
	f.mu.Lock()
	f.cancelled = true
	f.mu.Unlock()
	if r.executeJob(context.Background(), f.job, stream, start, false, 0) || executed.Load() || w.cancels != 1 || len(f.envelopes) != 1 || !reflect.DeepEqual(f.order, []string{"commit", "cancelled"}) {
		t.Fatal("queued job cancel not committed", f.order)
	}
}

func TestDurableAPIMultiRefusalDoesNotBackoffLegacyQueue(t *testing.T) {
	for _, payload := range []string{`{}`, `malformed-json`} {
		t.Run(payload, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/fabric/redis/jobs/consume" {
					t.Error("unexpected legacy disposition or control request")
					http.Error(w, "refused", 500)
					return
				}
				var req struct {
					Queue string `json:"queue"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				if req.Queue == "bad" {
					fmt.Fprintf(w, `{"messages":[{"id":"1-0","data":{"jobId":"refused","type":"CLOCK_FIXTURE","payload":%q,"protocol":"durable-results-v1"}}]}`, payload)
				} else {
					fmt.Fprint(w, `{"messages":[{"id":"2-0","data":{"jobId":"legacy","type":"CLOCK_FIXTURE","payload":"{}"}}]}`)
				}
			}))
			defer server.Close()
			source := NewAPISource(APISourceConfig{BaseURL: server.URL, Token: "synthetic", QueueNames: []string{"bad", "good"}, LogFn: func(string, string) {}})
			source.client = redisapi.NewClient(redisapi.ClientConfig{BaseURL: server.URL, Token: "synthetic"})
			defer source.Close()
			job, err := source.nextMulti(context.Background(), []string{"bad", "good"}, 1)
			if err != nil || job == nil || job.ID != "legacy" || source.RefusedDeliveries() != 1 {
				t.Fatal("refusal starved legacy work or triggered shared backoff", err, source.RefusedDeliveries())
			}
			job, err = source.nextMulti(context.Background(), []string{"bad"}, 1)
			if err != nil || job != nil || source.RefusedDeliveries() != 2 {
				t.Fatal("all-refused API poll fed fetch backoff", err, source.RefusedDeliveries())
			}
		})
	}
}
func TestDurableRedisFreshMultiBatchSkipsRefusalsAndReturnsLegacy(t *testing.T) {
	server := miniredis.RunT(t)
	ctx := context.Background()
	queues := []string{"jobs:v1:tag:bad-a", "jobs:v1:tag:bad-b", "jobs:v1:tag:good"}
	source := NewRedisSource(RedisSourceConfig{URL: "redis://" + server.Addr(), QueueNames: queues, ConsumerGroup: "fixture", LogFn: func(string, string) {}})
	if err := source.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	raw := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	defer raw.Close()
	for i, q := range queues {
		data := map[string]any{"jobId": "legacy", "type": "CLOCK_FIXTURE", "payload": "{}"}
		if i < 2 {
			data["protocol"] = "durable-results-v1"
			data["payload"] = "malformed-json"
		}
		if err := raw.XAdd(ctx, &goredis.XAddArgs{Stream: q, Values: data}).Err(); err != nil {
			t.Fatal(err)
		}
	}
	job, err := source.nextMulti(ctx, queues, -1)
	if err != nil || job == nil || job.ID != "legacy" || job.SourceQueue != queues[2] || source.RefusedDeliveries() != 2 {
		t.Fatal("fresh multi-batch lost legacy work behind refused entries", err, source.RefusedDeliveries())
	}
	for _, q := range queues {
		pending, err := raw.XPending(ctx, q, "fixture").Result()
		if err != nil || pending.Count != 1 {
			t.Fatal("multi-batch entry was disposed", q, err)
		}
	}
}

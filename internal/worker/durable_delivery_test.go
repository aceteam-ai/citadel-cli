package worker

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

type deliveryHTTP func(*http.Request) (*http.Response, error)

func (f deliveryHTTP) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type deliveryFixture struct {
	mu         sync.Mutex
	t          *testing.T
	source     *APISource
	job        *Job
	payload    string
	jobType    string
	cancelled  bool
	renewErr   bool
	commitErr  bool
	budget     int64
	renews     int
	envelopes  [][]byte
	order      []string
	unexpected []string
}

func newDeliveryFixture(t *testing.T, jobType string, payload string, configure ...func(*deliveryFixture)) *deliveryFixture {
	t.Helper()
	f := &deliveryFixture{t: t, jobType: jobType, payload: payload, budget: 300000}
	for _, apply := range configure {
		apply(f)
	}
	previous := http.DefaultTransport
	http.DefaultTransport = deliveryHTTP(f.request)
	t.Cleanup(func() { http.DefaultTransport = previous })
	f.source = NewAPISource(APISourceConfig{BaseURL: "https://fake.invalid", Token: "synthetic-device", QueueName: "jobs:v1:tag:fixture", LogFn: func(string, string) {}})
	f.source.client = redisapi.NewClient(redisapi.ClientConfig{BaseURL: "https://fake.invalid", Token: "synthetic-device"})
	t.Cleanup(func() { _ = f.source.Close() })
	job, err := f.source.nextSingle(context.Background(), f.source.queueNames[0], 1)
	if err != nil || job == nil || job.delivery == nil {
		t.Fatalf("fake delivery adoption failed: %v", err)
	}
	f.job = job
	return f
}
func (f *deliveryFixture) request(r *http.Request) (*http.Response, error) {
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	status := 200
	var body any
	switch {
	case r.URL.Path == "/api/fabric/redis/jobs/consume":
		var req redisapi.ConsumeRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			return nil, errors.New("bad fixture request")
		}
		if req.Protocol == "" {
			status = 409
			body = map[string]any{"error": "Queue protocol mismatch"}
			break
		}
		if req.Group != redisapi.DurableConsumerGroup {
			f.unexpected = append(f.unexpected, "wrong consume group")
		}
		digest := sha256.Sum256([]byte(f.payload))
		body = map[string]any{"messages": []any{map[string]any{"id": "100-0", "data": map[string]string{"jobId": "c486d0e4-9cad-4334-b437-2d5d847a5279", "type": f.jobType, "payload": f.payload, "protocol": redisapi.DurableResultsProtocol, "dispatchId": "c486d0e4-9cad-4334-b437-2d5d847a5279", "requestDigest": hex.EncodeToString(digest[:]), "claimToken": base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 32))), "leaseEpoch": "7"}}}}
	case strings.HasSuffix(r.URL.Path, "/renew"):
		f.renews++
		if f.renewErr {
			status = 403
			body = map[string]any{"error": "refused"}
			break
		}
		var tuple map[string]any
		_ = json.NewDecoder(r.Body).Decode(&tuple)
		if tuple["leaseEpoch"] != float64(7) || r.Header.Get("x-claim-token") == "" {
			f.unexpected = append(f.unexpected, "changed renewal tuple")
		}
		digest := sha256.Sum256([]byte(f.payload))
		body = map[string]any{"dispatchId": "c486d0e4-9cad-4334-b437-2d5d847a5279", "requestDigest": hex.EncodeToString(digest[:]), "leaseEpoch": 7, "expiresAt": "2001-01-01T00:00:00Z", "remainingLeaseMs": f.budget}
	case strings.HasSuffix(r.URL.Path, "/complete"):
		raw, _ := io.ReadAll(r.Body)
		f.envelopes = append(f.envelopes, raw)
		f.order = append(f.order, "commit")
		if r.URL.Query().Get("leaseEpoch") != "7" || r.URL.Query().Get("messageId") != "100-0" || r.Header.Get("x-claim-token") == "" {
			f.unexpected = append(f.unexpected, "changed completion tuple")
		}
		if f.commitErr {
			return nil, errors.New("ambiguous completion outcome")
		}
		body = map[string]any{"dispatchId": "c486d0e4-9cad-4334-b437-2d5d847a5279", "status": "committed", "envelopeSha256": strings.Repeat("b", 64), "committedAt": "2026-10-28T00:00:00.000001Z", "payloadExpiresAt": "2026-10-29T00:00:00.000001Z", "outboxId": "100-0"}
	case r.URL.Path == "/api/fabric/redis/pubsub/publish":
		var request struct {
			Message string `json:"message"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			return nil, errors.New("bad publish fixture")
		}
		var event redisapi.StreamEvent
		if json.Unmarshal([]byte(request.Message), &event) != nil {
			return nil, errors.New("bad event fixture")
		}
		f.order = append(f.order, event.Type)
		body = map[string]any{"published": true}
	case r.URL.Path == "/api/fabric/redis/kv" && r.Method == http.MethodGet && strings.HasPrefix(r.URL.Query().Get("key"), "job:cancelled:"):
		if f.cancelled {
			body = map[string]any{"value": "1", "ttl": 60}
		} else {
			body = map[string]any{"value": nil, "ttl": -2}
		}
	default:
		f.unexpected = append(f.unexpected, r.URL.Path)
		return nil, errors.New("unexpected live/legacy route")
	}
	raw, _ := json.Marshal(body)
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
}
func (f *deliveryFixture) check(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.unexpected) > 0 {
		t.Fatal("unexpected route or tuple", f.unexpected)
	}
}

type deliveryWriter struct {
	NoOpStreamWriter
	f                   *deliveryFixture
	ends, errs, cancels int
	result              map[string]any
}

func (w *deliveryWriter) WriteEnd(v map[string]any) error {
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	w.ends++
	w.result = v
	w.f.order = append(w.f.order, "end")
	return nil
}
func (w *deliveryWriter) WriteError(error, bool) error {
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	w.errs++
	w.f.order = append(w.f.order, "error")
	return nil
}
func (w *deliveryWriter) WriteCancelled(string) error {
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	w.cancels++
	w.f.order = append(w.f.order, "cancelled")
	return nil
}
func (f *deliveryFixture) runner(h JobHandler) (*Runner, *deliveryWriter) {
	w := &deliveryWriter{f: f}
	var handlers []JobHandler
	if h != nil {
		handlers = []JobHandler{h}
	}
	r := NewRunner(f.source, handlers, RunnerConfig{WorkerID: "fake", NodeID: "123", ActivityFn: func(string, string) {}})
	r.WithStreamWriterFactory(func(*Job) StreamWriter { return w })
	return r, w
}
func TestDurableRunnerCommitsBeforePublishingAndIgnoresHandlerTerminal(t *testing.T) {
	f := newDeliveryFixture(t, "CLOCK_FIXTURE", `{}`)
	h := &clockFixtureHandler{run: func(ctx context.Context, j *Job, s StreamWriter) (*JobResult, error) {
		_ = s.WriteEnd(map[string]any{"content": "premature"})
		_ = s.WriteError(errors.New("premature"), false)
		_ = s.WriteCancelled("premature")
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.order) != 0 || len(f.envelopes) != 0 {
			t.Fatal("handler terminal escaped before completion")
		}
		return &JobResult{Status: JobStatusSuccess, Output: map[string]any{"content": "finished"}}, nil
	}}
	r, w := f.runner(h)
	proceed, stream, start := r.claimJob(context.Background(), f.job)
	if !proceed || !r.executeJob(context.Background(), f.job, stream, start, false, 0) {
		t.Fatal("valid delivery failed")
	}
	if !reflect.DeepEqual(f.order, []string{"commit", "end"}) || w.ends != 1 || w.errs != 0 || w.cancels != 0 || w.result["content"] != "finished" {
		t.Fatal("terminal escaped receipt barrier", f.order)
	}
	var envelope map[string]any
	_ = json.Unmarshal(f.envelopes[0], &envelope)
	if envelope["result"].(map[string]any)["content"] != "finished" {
		t.Fatal("committed different output")
	}
	f.check(t)
}
func TestDurableRunnerUnknownCommitPublishesNothingAndNeverLegacyAcks(t *testing.T) {
	f := newDeliveryFixture(t, "CLOCK_FIXTURE", `{}`)
	f.commitErr = true
	h := &clockFixtureHandler{run: func(context.Context, *Job, StreamWriter) (*JobResult, error) {
		return &JobResult{Status: JobStatusSuccess, Output: map[string]any{"content": "finished"}}, nil
	}}
	r, w := f.runner(h)
	_, stream, start := r.claimJob(context.Background(), f.job)
	if r.executeJob(context.Background(), f.job, stream, start, false, 0) {
		t.Fatal("unknown commit reported success")
	}
	if len(f.envelopes) != 1 || w.ends+w.errs+w.cancels != 0 || f.job.delivery().valid() {
		t.Fatal("unknown commit published or retained execution authority")
	}
	// Repeated completion calls cannot acquire a new epoch or choose a new body.
	if err := f.source.Fail(context.Background(), f.job, errors.New("different"), nil); err == nil || len(f.envelopes) != 1 {
		t.Fatal("lost claim was repaired/reused")
	}
	f.check(t)
}
func TestDurableTerminalMappings(t *testing.T) {
	for _, name := range []string{"cancel-before", "cancel-during", "cancel-result", "permanent", "unsupported", "warming", "retry", "generic-failure", "unresolved"} {
		t.Run(name, func(t *testing.T) {
			typ := "CLOCK_FIXTURE"
			if name == "unsupported" {
				typ = "UNKNOWN_FIXTURE"
			}
			f := newDeliveryFixture(t, typ, `{}`)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h := &clockFixtureHandler{run: func(ctx context.Context, j *Job, s StreamWriter) (*JobResult, error) {
				switch name {
				case "cancel-during":
					cancel()
					<-ctx.Done()
					return nil, ctx.Err()
				case "cancel-result":
					return &JobResult{Status: JobStatusCancelled}, nil
				case "permanent":
					return &JobResult{Status: JobStatusTerminalFailure, Error: errors.New("permanent fixture")}, nil
				case "warming":
					return &JobResult{Status: JobStatusSuccess, Output: map[string]any{"status": "model_warming"}}, nil
				case "retry":
					return &JobResult{Status: JobStatusRetry}, nil
				case "generic-failure":
					return nil, errors.New("transient fixture")
				}
				return &JobResult{Status: JobStatusSuccess}, nil
			}}
			var handler JobHandler = h
			if name == "unsupported" {
				handler = nil
			}
			r, w := f.runner(handler)
			if name == "unresolved" {
				r.config.NodeID = ""
			}
			f.cancelled = name == "cancel-before"
			proceed, stream, start := r.claimJob(context.Background(), f.job)
			if proceed {
				_ = r.executeJob(ctx, f.job, stream, start, false, 0)
			}
			switch name {
			case "cancel-during", "warming", "retry", "generic-failure":
				if len(f.envelopes) != 0 || w.ends+w.errs+w.cancels != 0 || f.job.delivery().valid() {
					t.Fatal("retry became terminal", f.order)
				}
			default:
				if len(f.envelopes) != 1 {
					t.Fatal("missing durable terminal", f.order)
				}
				var envelope map[string]map[string]any
				_ = json.Unmarshal(f.envelopes[0], &envelope)
				if strings.HasPrefix(name, "cancel-") && envelope["error"]["code"] != "cancelled" {
					t.Fatal("wrong cancellation mapping", envelope)
				}
				if name == "unresolved" && envelope["error"]["code"] != "target_unresolved" {
					t.Fatal("unresolved target not committed", envelope)
				}
				if len(f.order) != 2 || f.order[0] != "commit" {
					t.Fatal("terminal before receipt", f.order)
				}
			}
			f.check(t)
		})
	}
}
func TestDurableLeaseLossCancelsAndRetainsActualHandlerCustody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryFixture(t, "CLOCK_FIXTURE", `{"timeout_ms":10000}`)
		started, cancelled, exit := make(chan struct{}), make(chan struct{}), make(chan struct{})
		h := &clockFixtureHandler{run: func(ctx context.Context, _ *Job, s StreamWriter) (*JobResult, error) {
			close(started)
			<-ctx.Done()
			close(cancelled)
			<-exit
			_ = s.WriteEnd(map[string]any{"late": true})
			return &JobResult{Status: JobStatusSuccess}, nil
		}}
		r, w := f.runner(h)
		_, stream, start := r.claimJob(context.Background(), f.job)
		done := make(chan bool, 1)
		go func() {
			done <- r.runLaneJob(context.Background(), r.unboundedLane, f.job, stream, start, r.unboundedLane.reserveExecTicket())
		}()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("handler did not start")
		}
		f.job.delivery().stop()
		select {
		case <-cancelled:
		case <-time.After(time.Second):
			t.Fatal("handler not cancelled on lease loss")
		}
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("custody released before handler exit")
		default:
		}
		if len(r.unboundedLane.exec) != 1 || r.unboundedLane.snapshot().Executing != 1 {
			t.Fatal("lost handler released execution lane before actual exit")
		}
		close(exit)
		select {
		case ok := <-done:
			if ok {
				t.Fatal("lost delivery succeeded")
			}
		case <-time.After(time.Second):
			t.Fatal("actual exit did not release custody")
		}
		if w.ends+w.errs+w.cancels != 0 || len(f.envelopes) != 0 {
			t.Fatal("late result escaped lost epoch")
		}
		f.check(t)

	})
}

type fakeDeliveryControl struct {
	renew    func(context.Context) (time.Duration, error)
	complete func(context.Context, []byte) error
}

func (f fakeDeliveryControl) RenewDelivery(ctx context.Context, _ *redisapi.DeliveryClaim) (time.Duration, error) {
	return f.renew(ctx)
}
func (f fakeDeliveryControl) CompleteDelivery(ctx context.Context, _ *redisapi.DeliveryClaim, b []byte) error {
	if f.complete != nil {
		return f.complete(ctx, b)
	}
	return nil
}
func TestDurableRenewalChargesRoundTripAndNeverUsesAdvisoryWallTime(t *testing.T) {
	now := time.Now()
	lost, lose := context.WithCancel(context.Background())
	loop, stop := context.WithCancel(context.Background())
	d := &durableDelivery{now: func() time.Time { return now }, lost: lost, lose: lose, loop: loop, stopLoop: stop}
	d.control = fakeDeliveryControl{renew: func(context.Context) (time.Duration, error) {
		now = now.Add(1100 * time.Millisecond)
		return time.Second, nil
	}}
	if err := d.renew(context.Background()); err == nil || d.valid() || lost.Err() == nil {
		t.Fatal("response latency created extra lease time")
	}
}
func TestDurableRenewalRunsWhileQueuedAndSourceCloseLosesLease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var renews atomic.Int64
		renewed := make(chan struct{}, 1)
		c := fakeDeliveryControl{renew: func(context.Context) (time.Duration, error) {
			if renews.Add(1) >= 2 {
				select {
				case renewed <- struct{}{}:
				default:
				}
			}
			return 80 * time.Millisecond, nil
		}}
		d, err := startDurableDelivery(context.Background(), c, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer d.stop()
		// No handler has started. The renewal loop belongs to the delivery, not Execute.
		select {
		case <-renewed:
		case <-time.After(time.Second):
			t.Fatal("queued delivery did not renew at half-life")
		}
		f := newDeliveryFixture(t, "CLOCK_FIXTURE", `{}`)
		if err := f.source.Close(); err != nil {
			t.Fatal(err)
		}
		if f.job.delivery().valid() || f.job.delivery().lost.Err() == nil {
			t.Fatal("Close left live delivery authority")
		}

	})
}
func TestDurableExpiredLeaseCancelsEvenWhenRenewalIgnoresContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int64
		blocked, release := make(chan struct{}), make(chan struct{})
		c := fakeDeliveryControl{renew: func(context.Context) (time.Duration, error) {
			if calls.Add(1) > 1 {
				close(blocked)
				<-release
			}
			return 80 * time.Millisecond, nil
		}}
		d, err := startDurableDelivery(context.Background(), c, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer d.stop()
		defer close(release)
		<-blocked
		select {
		case <-d.lost.Done():
		case <-time.After(time.Second):
			t.Fatal("blocked renewal extended execution past deadline")
		}
		if d.valid() {
			t.Fatal("expired claim still valid")
		}

	})
}
func TestDurableNonHTTPMetadataRefusedIncludingMalformedAndCaseAliases(t *testing.T) {
	for _, key := range []string{"protocol", "Protocol", "dispatchId", "requestDigest", "claimToken", "leaseEpoch"} {
		if !hasDurableCarrier(map[string]any{key: nil}) {
			t.Fatalf("direct Redis carrier not refused: %s", key)
		}
		s := NewWSSource(WSSourceConfig{})
		if job, err := s.convertWSJob(redisapi.WSMessage{Data: map[string]string{key: "", "payload": "{}", "jobId": "fixture"}}); err == nil || job != nil {
			t.Fatalf("WS carrier admitted: %s", key)
		}
	}
	if hasDurableCarrier(map[string]any{"payload": map[string]any{"protocol": redisapi.DurableResultsProtocol}}) {
		t.Fatal("payload spoof enabled durable mode")
	}
}
func TestDurableCommitIntentIsImmutableUnderCallerMutation(t *testing.T) {
	f := newDeliveryFixture(t, "CLOCK_FIXTURE", `{}`)
	r, w := f.runner(nil)
	stream := r.newStreamWriter(f.job)
	output := map[string]any{"content": "original"}
	_ = stream.WriteEnd(output)
	output["content"] = "changed"
	_ = stream.WriteEnd(output)
	if err := f.source.Ack(context.Background(), f.job); err != nil {
		t.Fatal(err)
	}
	if w.result["content"] != "original" || !strings.Contains(string(f.envelopes[0]), `"original"`) {
		t.Fatal("terminal changed after staging")
	}
	f.check(t)
}

func TestDurableImmediateRenewalFailureNeverAdmitsOrAcknowledges(t *testing.T) {
	for _, name := range []string{"denied", "zero-budget", "worker-control", "huddle", "meeting"} {
		t.Run(name, func(t *testing.T) {
			// Set up the fake transport without asking the fixture helper to admit a job.
			f := &deliveryFixture{t: t, jobType: "CLOCK_FIXTURE", payload: `{}`, budget: 300000}
			switch name {
			case "denied":
				f.renewErr = true
			case "zero-budget":
				f.budget = 0
			case "worker-control":
				f.jobType = JobTypeWorkerControl
			case "huddle":
				f.jobType = JobTypeHuddleJoin
			case "meeting":
				f.jobType = JobTypeMeetingJoin
			}
			old := http.DefaultTransport
			http.DefaultTransport = deliveryHTTP(f.request)
			defer func() { http.DefaultTransport = old }()
			source := NewAPISource(APISourceConfig{QueueName: "jobs:v1:tag:fixture"})
			source.client = redisapi.NewClient(redisapi.ClientConfig{BaseURL: "https://fake.invalid", Token: "synthetic-device"})
			defer source.Close()
			job, err := source.nextSingle(context.Background(), "jobs:v1:tag:fixture", 1)
			if err != nil || job != nil || source.RefusedDeliveries() != 1 || len(f.envelopes) != 0 {
				t.Fatal("unrenewed or unsupported delivery admitted")
			}
			if name == "denied" || name == "zero-budget" {
				if f.renewalCount() != 1 {
					t.Fatal("first renewal was not immediate")
				}
			}
			f.check(t)
		})
	}
}

func TestDurableRealLaneWaitRenewsAndLeaseLossDoesNotStartHandler(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryFixture(t, "CLOCK_FIXTURE", `{}`, func(f *deliveryFixture) { f.budget = 1000 })
		var started atomic.Bool
		h := &clockFixtureHandler{run: func(context.Context, *Job, StreamWriter) (*JobResult, error) {
			started.Store(true)
			return &JobResult{Status: JobStatusSuccess}, nil
		}}
		r, _ := f.runner(h)
		_, stream, start := r.claimJob(context.Background(), f.job)
		lane := r.unboundedLane
		lane.exec <- struct{}{} // another real lane owner holds the execution slot
		ticket := lane.reserveExecTicket()
		done := make(chan bool, 1)
		go func() { done <- r.runLaneJob(context.Background(), lane, f.job, stream, start, ticket) }()
		time.Sleep(600 * time.Millisecond)
		if f.renewalCount() < 2 || started.Load() {
			t.Fatal("waiting delivery failed to renew or overlapped lane owner")
		}
		f.mu.Lock()
		f.renewErr = true
		f.mu.Unlock()
		time.Sleep(600 * time.Millisecond)
		if ok := <-done; ok || started.Load() {
			t.Fatal("lease-lost waiter executed")
		}
		<-lane.exec
		if len(f.envelopes) != 0 {
			t.Fatal("lease-lost waiter committed terminal")
		}
		f.check(t)
	})
}
func TestDurableLaneSaturationAndQueueWarmingLapseWithoutTerminal(t *testing.T) {
	for _, name := range []string{"saturated", "queue-warming"} {
		t.Run(name, func(t *testing.T) {
			f := newDeliveryFixture(t, "CLOCK_FIXTURE", `{}`)
			r, w := f.runner(nil)
			_, stream, start := r.claimJob(context.Background(), f.job)
			if name == "saturated" {
				lane := r.unboundedLane
				for i := 0; i < cap(lane.admit); i++ {
					lane.admit <- struct{}{}
				}
				var wg sync.WaitGroup
				r.dispatchLane(context.Background(), lane, f.job, stream, start, &wg)
				wg.Wait()
			} else {
				if r.finishQueueWaitExceeded(context.Background(), f.job, stream, time.Now()) {
					t.Fatal("warming counted as durable success")
				}
			}
			if f.job.delivery().valid() || len(f.envelopes) != 0 || w.ends+w.errs+w.cancels != 0 {
				t.Fatal("backpressure became terminal")
			}
			f.check(t)
		})
	}
}
func TestDurableClockBudgetAndEpochCannotBeRefreshedAfterLoss(t *testing.T) {
	f := newDeliveryFixture(t, "CLOCK_FIXTURE", `{}`)
	d := f.job.delivery()
	d.stop()
	before := f.renewalCount()
	var called atomic.Bool
	d.control = fakeDeliveryControl{renew: func(context.Context) (time.Duration, error) { called.Store(true); return time.Second, nil }}
	if err := d.renew(context.Background()); err == nil || d.valid() {
		t.Fatal("lost original claim was renewed")
	}
	// A locally lost claim must not even ask the server to restore authority.
	if f.renewalCount() != before || called.Load() {
		t.Fatal("reacquisition attempted after loss")
	}
	f.check(t)
}

func TestDurableRealAPIWriterCommitsVerifiedJobCancellation(t *testing.T) {
	f := newDeliveryFixture(t, "CLOCK_FIXTURE", `{}`)
	root, cancel := context.WithCancel(context.Background())
	h := &clockFixtureHandler{run: func(ctx context.Context, _ *Job, s StreamWriter) (*JobResult, error) {
		_ = s.WriteProgress(map[string]any{"count": 1})
		_ = s.WriteEnd(map[string]any{"early": true})
		return &JobResult{Status: JobStatusCancelled}, nil
	}}
	defer cancel()
	r, _ := f.runner(h)
	r.WithStreamWriterFactory(CreateAPIStreamWriterFactory(root, f.source))
	_, stream, start := r.claimJob(root, f.job)
	if r.executeJob(root, f.job, stream, start, false, 0) {
		t.Fatal("cancelled job succeeded")
	}
	if !reflect.DeepEqual(f.order, []string{"claimed", "start", "progress", "commit", "cancelled"}) {
		t.Fatal("real API writer order/legacy side effects", f.order)
	}
	f.check(t)
}

func TestDurableQueuedWorkerShutdownLapsesWithoutTerminal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryFixture(t, "CLOCK_FIXTURE", `{}`)
		r, w := f.runner(nil)
		_, stream, start := r.claimJob(context.Background(), f.job)
		lane := r.unboundedLane
		lane.exec <- struct{}{}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan bool, 1)
		go func() { done <- r.runLaneJob(ctx, lane, f.job, stream, start, lane.reserveExecTicket()) }()
		synctest.Wait()
		cancel()
		if <-done {
			t.Fatal("cancelled waiter succeeded")
		}
		<-lane.exec
		if w.ends+w.errs+w.cancels != 0 || len(f.envelopes) != 0 || f.job.delivery().valid() {
			t.Fatal("worker shutdown became a queued terminal", f.order)
		}
		f.check(t)
	})
}

func TestDurableDirectRedisRefusalLeavesPendingAndNeverDeadLetters(t *testing.T) {
	for _, multi := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "multi"}[multi], func(t *testing.T) {
			server := miniredis.RunT(t)
			queue := "jobs:v1:tag:durable-fixture"
			source := NewRedisSource(RedisSourceConfig{URL: "redis://" + server.Addr(), QueueName: queue, ConsumerGroup: "fixture-workers", MaxAttempts: 1, LogFn: func(string, string) {}})
			ctx := context.Background()
			if err := source.Connect(ctx); err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			redis := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
			defer redis.Close()
			if multi {
				if err := redis.XGroupCreateMkStream(ctx, "jobs:v1:tag:empty", "fixture-workers", "0").Err(); err != nil {
					t.Fatal(err)
				}
			}
			if err := redis.XAdd(ctx, &goredis.XAddArgs{Stream: queue, Values: map[string]any{"jobId": "fixture", "type": "CLOCK_FIXTURE", "payload": "{}", "protocol": redisapi.DurableResultsProtocol, "claimToken": "synthetic-unsupported"}}).Err(); err != nil {
				t.Fatal(err)
			}
			var job *Job
			var err error
			if multi {
				job, err = source.nextMulti(ctx, []string{queue, "jobs:v1:tag:empty"}, -1)
			} else {
				job, err = source.nextSingle(ctx, queue, -1)
			}
			if err != nil || job != nil || source.RefusedDeliveries() != 1 {
				t.Fatal("durable refusal became a fetch error or execution")
			}
			pending, err := redis.XPending(ctx, queue, "fixture-workers").Result()
			if err != nil || pending.Count != 1 {
				t.Fatal("unsupported v1 entry was ACKed", pending, err)
			}
			for _, key := range server.Keys() {
				if strings.Contains(strings.ToLower(key), "dlq") {
					t.Fatal("unsupported v1 entry was dead-lettered")
				}
			}
		})
	}
}

func startDurableDelivery(ctx context.Context, control deliveryControl, claim *redisapi.DeliveryClaim) (*durableDelivery, error) {
	d := newDurableDelivery(control, claim)
	if err := d.renew(ctx); err != nil {
		d.stop()
		return nil, err
	}
	go d.keepAlive()
	return d, nil
}

func TestDurableLateReceiptAfterLeaseLossNeverPublishes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int64
		control := fakeDeliveryControl{
			renew: func(context.Context) (time.Duration, error) {
				if calls.Add(1) > 1 {
					return 0, errors.New("lost")
				}
				return time.Second, nil
			},
			complete: func(context.Context, []byte) error { time.Sleep(2 * time.Second); return nil }, // deliberately ignores the cancelled HTTP context
		}
		d, err := startDurableDelivery(context.Background(), control, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer d.stop()
		f := &deliveryFixture{}
		w := &deliveryWriter{f: f}
		d.raw = w
		if err := d.stage(deliveryTerminal{kind: "result", output: json.RawMessage(`{"late":true}`)}); err != nil {
			t.Fatal(err)
		}
		if err := d.finish(context.Background(), nil, ""); err == nil || d.isCommitted() || w.ends != 0 {
			t.Fatal("late receipt regained lost epoch")
		}
	})
}

func TestDurableWorkerJobFormattingCannotReachControlCredentials(t *testing.T) {
	f := newDeliveryFixture(t, "CLOCK_FIXTURE", `{}`)
	token := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%#x", "%d"} {
		for _, parent := range []any{f.job, *f.job, []Job{*f.job}, map[string]Job{"fixture": *f.job}} {
			text := fmt.Sprintf(verb, parent)
			for _, secret := range []string{token, "synthetic-device"} {
				if strings.Contains(text, secret) {
					t.Fatal("job formatting reached a delivery credential", verb)
				}
			}
		}
	}
}

type deliveryEndHook struct {
	NoOpStreamWriter
	end func() error
}

func (w deliveryEndHook) WriteEnd(map[string]any) error { return w.end() }
func TestDurableCommittedReceiptOwnsOverlappingRenewalCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int64
		renewalStarted := make(chan struct{})
		control := fakeDeliveryControl{renew: func(ctx context.Context) (time.Duration, error) {
			if calls.Add(1) == 1 {
				return time.Second, nil
			}
			close(renewalStarted)
			<-ctx.Done()
			return 0, ctx.Err()
		}}
		d, err := startDurableDelivery(context.Background(), control, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer d.stop()
		time.Sleep(600 * time.Millisecond)
		<-renewalStarted
		publishing, release := make(chan struct{}), make(chan struct{})
		d.raw = &deliveryEndHook{end: func() error { close(publishing); <-release; return nil }}
		_ = d.stage(deliveryTerminal{kind: "result", output: json.RawMessage(`{}`)})
		done := make(chan error, 1)
		go func() { done <- d.finish(context.Background(), nil, "") }()
		<-publishing
		synctest.Wait()
		revoked := !d.isCommitted() || d.lost.Err() != nil
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if revoked {
			t.Fatal("overlapping renewal revoked an already committed terminal publisher")
		}
	})
}

// Use the real legacy adapter: cooperative handlers return a successful byte
// result with partial counts rather than JobStatusCancelled.
type deliveryCooperativeLegacy struct {
	run func(jobs.JobContext) ([]byte, error)
}

func (h deliveryCooperativeLegacy) Execute(ctx jobs.JobContext, _ *nexus.Job) ([]byte, error) {
	return h.run(ctx)
}
func (h deliveryCooperativeLegacy) Capabilities() jobs.JobCapabilities {
	return jobs.JobCapabilities{CooperativeCancel: true}
}
func TestDurableLegacyCooperativeCancellationCommitsCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeliveryFixture(t, "CLOCK_FIXTURE", `{}`)
		partialCommitted := false
		legacy := deliveryCooperativeLegacy{run: func(ctx jobs.JobContext) ([]byte, error) {
			f.mu.Lock()
			f.cancelled = true
			f.mu.Unlock()
			for !ctx.CancelRequestedNow() {
				time.Sleep(time.Millisecond)
			}
			if ctx.Context().Err() != nil {
				t.Error("graceful cancellation aborted the current file")
			}
			partialCommitted = true
			return []byte(`{"status":"cancelled","files_indexed":1}`), nil
		}}
		adapter := NewLegacyHandlerAdapter("CLOCK_FIXTURE", legacy)
		r, w := f.runner(adapter)
		r.coopCancelPoll = time.Millisecond
		_, stream, start := r.claimJob(context.Background(), f.job)
		if r.executeJob(context.Background(), f.job, stream, start, false, 0) {
			t.Error("graceful cancellation reported a successful durable result")
		}
		if !partialCommitted || len(f.envelopes) != 1 || w.cancels != 1 || w.ends != 0 {
			t.Fatal("cooperative cancellation lost partial-file custody or terminal mapping", f.order)
		}
		var envelope map[string]map[string]any
		_ = json.Unmarshal(f.envelopes[0], &envelope)
		if envelope["error"]["code"] != "cancelled" {
			t.Fatal("cooperative cancellation did not commit cancelled", envelope)
		}
		f.check(t)
	})
}

func (f *deliveryFixture) renewalCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.renews }

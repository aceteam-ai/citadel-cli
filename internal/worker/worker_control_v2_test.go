package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	wc "github.com/aceteam-ai/citadel-cli/internal/workercontrol"
	"github.com/aceteam-ai/citadel-cli/internal/workercontrol/controltest"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

func TestWorkerControlRejectedWebSocketClaimCannotSchedule(t *testing.T) {
	for _, mode := range []string{"denied", "silent", "generic_error", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			f := controltest.Load()
			f.Identity.AttemptID = uuid.NewString()
			f.Identity.ExpiresAt = time.Now().Add(time.Minute).UnixMilli()
			var schedules, commits, generic atomic.Int32
			upgrader := websocket.Upgrader{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer synthetic-token" {
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
					_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{map[string]any{"id": f.Deliveries["A"].MessageID, "data": f.Data()}}})
				case "/api/fabric/redis/ws":
					conn, e := upgrader.Upgrade(w, r, nil)
					if e != nil {
						return
					}
					defer conn.Close()
					var frame struct {
						Type    string     `json:"type"`
						Control wc.Request `json:"control"`
					}
					if conn.ReadJSON(&frame) != nil {
						return
					}
					if frame.Type != "worker_control_request" || frame.Control.Operation != "claim_and_ack" {
						t.Error("wrong control operation")
						return
					}
					response := controltest.Accept(frame.Control, time.Now().UnixMilli())
					response.Decision = "denied"
					response.Code = "expired"
					response.Receipt = nil
					response.Observation = nil
					if mode == "denied" {
						_ = conn.WriteJSON(map[string]any{"type": "worker_control_response", "control": response})
					}
					if mode == "unavailable" {
						response.Decision = "unavailable"
						response.Code = "authority_unavailable"
						_ = conn.WriteJSON(map[string]any{"type": "worker_control_response", "control": response})
					}
					if mode == "generic_error" {
						_ = conn.WriteJSON(map[string]any{"type": "error", "error": "Failed to publish message"})
					}
					var discard any
					_ = conn.ReadJSON(&discard)
				default:
					generic.Add(1)
					w.WriteHeader(403)
				}
			}))
			defer srv.Close()
			api := NewAPISource(APISourceConfig{BaseURL: srv.URL, Token: "synthetic-token", QueueName: DefaultCPUQueue})
			ctx := context.Background()
			if e := api.Connect(ctx); e != nil {
				t.Fatal(e)
			}
			defer api.Close()
			control, e := api.WorkerControlSource(ctx, fixtureOrg, "758")
			if e != nil || control == nil {
				t.Fatalf("source=%v err=%v", control, e)
			}
			if e := control.Connect(ctx); e != nil {
				t.Fatal(e)
			}
			defer control.Close()
			if e := control.Client().EnableWebSocket(ctx); e != nil {
				t.Fatal(e)
			}
			job, e := control.Next(ctx)
			if e != nil {
				t.Fatal(e)
			}
			h := NewWorkerControlHandler(WorkerControlConfig{OrgID: fixtureOrg, NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Schedule: func(func() bool) (func(), func(), error) {
				schedules.Add(1)
				return func() { commits.Add(1) }, func() {}, nil
			}})
			runCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
			defer cancel()
			runner := NewRunner(control, []JobHandler{h}, RunnerConfig{NodeID: "758"})
			runner.WithStreamWriterFactory(CreateAPIStreamWriterFactory(runCtx, control))
			if proceed, _, _ := runner.claimJob(runCtx, job); proceed {
				t.Fatal("unconfirmed claim admitted execution")
			}
			if schedules.Load() != 0 || commits.Load() != 0 || generic.Load() != 0 {
				t.Fatalf("claim without authority caused schedule=%d commit=%d generic=%d", schedules.Load(), commits.Load(), generic.Load())
			}
			// A second delivery on the same process cannot reconstruct the cancelled
			// pipeline from an acceptance that might subsequently arrive.
			again, e := control.Next(ctx)
			if e != nil {
				t.Fatal(e)
			}
			if again.controlSession != job.controlSession || again.controlSession.CanPrepare() {
				t.Fatal("redelivery reopened cancelled claim")
			}
		})
	}
}

func TestWorkerControlDelayedExitChecksPermitAtCallback(t *testing.T) {
	checked := make(chan struct{})
	exited := make(chan struct{}, 1)
	commit, _, err := PrepareWorkerRestart(func() { exited <- struct{}{} }, func() bool { close(checked); return false })
	if err != nil {
		t.Fatal(err)
	}
	commit()
	select {
	case <-checked:
	case <-time.After(2 * time.Second):
		t.Fatal("delayed exit never checked permit")
	}
	select {
	case <-exited:
		t.Fatal("expired delayed exit executed")
	default:
	}
}

func TestWorkerControlPreparedGateCancellationIsIrreversible(t *testing.T) {
	count, cancels := 0, 0
	h := NewWorkerControlHandler(WorkerControlConfig{OrgID: fixtureOrg, NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Schedule: func(func() bool) (func(), func(), error) { return func() { count++ }, func() { cancels++ }, nil }})
	job := controlJob("cancel-gate")
	events := []string{}
	writer := &controlWriter{events: &events}
	writer.authenticatedWorkerControl(job)
	result, e := h.Execute(context.Background(), job, writer)
	if e != nil || result.Output["accepted"] != true {
		t.Fatal("reservation")
	}
	commit, cancel, e := h.PrepareAfterAck(job)
	if e != nil {
		t.Fatal(e)
	}
	cancel()
	commit()
	cancel()
	if count != 0 || cancels != 1 || job.controlSession.CanCommit() {
		t.Fatalf("cancelled gate count=%d cancel=%d", count, cancels)
	}
	if writer.WriteEnd(map[string]any{"accepted": true}) == nil {
		t.Fatal("late completion revived cancelled gate")
	}
	if _, _, e := h.PrepareAfterAck(job); e == nil {
		t.Fatal("cancelled gate reconstructed")
	}
}

func TestWorkerControlCommitIntentPersistenceFailureCannotOpenGate(t *testing.T) {
	count, cancels := 0, 0
	state := filepath.Join(t.TempDir(), "state")
	h := NewWorkerControlHandler(WorkerControlConfig{OrgID: fixtureOrg, NodeID: "758", StateDir: state, Managed: func() bool { return true }, Schedule: func(func() bool) (func(), func(), error) { return func() { count++ }, func() { cancels++ }, nil }})
	job := controlJob("persist-commit")
	events := []string{}
	writer := &controlWriter{events: &events}
	writer.authenticatedWorkerControl(job)
	if result, err := h.Execute(context.Background(), job, writer); err != nil || result.Output["accepted"] != true {
		t.Fatal("reservation")
	}
	commit, _, err := h.PrepareAfterAck(job)
	if err != nil {
		t.Fatal(err)
	}
	if writer.WriteEnd(map[string]any{"accepted": true}) != nil {
		t.Fatal("permit")
	}
	if err := os.Rename(state, state+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	commit()
	if count != 0 || cancels != 1 || job.controlSession.CanCommit() {
		t.Fatal("failed commit-intent persistence authorized exit")
	}
}

func TestWorkerControlWSOptInRequiresActualDeviceToken(t *testing.T) {
	s := NewWSSource(WSSourceConfig{Client: redisapi.NewClient(redisapi.ClientConfig{})})
	defer s.Client().Close()
	if control, err := s.WorkerControlSource(context.Background(), fixtureOrg, "758"); err != nil || control != nil {
		t.Fatal("tokenless WS source attempted control opt-in")
	}
}

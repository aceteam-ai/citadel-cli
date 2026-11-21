package workercontrol

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

type sessionFixture struct {
	Identity     Identity               `json:"identity"`
	Deliveries   map[string]Delivery    `json:"deliveries"`
	Process      string                 `json:"process_id"`
	Receipts     map[string]Receipt     `json:"receipts"`
	Observations map[string]Observation `json:"observations"`
}

func sf(t *testing.T) sessionFixture {
	t.Helper()
	raw, e := os.ReadFile("controltest/testdata/wire.json")
	if e != nil {
		t.Fatal(e)
	}
	var f sessionFixture
	if json.Unmarshal(raw, &f) != nil {
		t.Fatal("fixture")
	}
	return f
}
func (f sessionFixture) response(r Request, server int64) Response {
	p := f.Receipts["claim"]
	o := f.Observations["claim"]
	if r.Operation == "complete" || r.Operation == "read_status" {
		p = f.Receipts["complete"]
		o = f.Observations["complete"]
	}
	p.OperationDigest = r.OperationDigest
	p.ProcessID = r.ProcessID
	p.Delivery = r.Delivery
	if r.Operation == "complete" {
		p.Outcome = r.Outcome
		p.Code = r.Code
		p.OutcomeDigest = r.OutcomeDigest
		p.ClaimEpoch = r.ClaimEpoch
	}
	o.ServerTimeMS = server
	return Response{RequestID: r.RequestID, Operation: r.Operation, Identity: r.Identity, Delivery: r.Delivery, ProcessID: r.ProcessID, OperationDigest: r.OperationDigest, RequestDigest: r.RequestDigest, Decision: "accepted", Code: "ok", Receipt: &p, Observation: &o}
}
func TestWorkerControlDeadlineNeverRenewsFromReplayedReceipt(t *testing.T) {
	f := sf(t)
	now := time.Now()
	clock := func() time.Time { return now }
	server := int64(1800000000001)
	s := newSession(Envelope{f.Identity, f.Deliveries["A"]}, func(ctx context.Context, r Request) (Response, error) {
		if r.Operation == "complete" {
			server = 1800000001001
		}
		return f.response(r, server), nil
	}, clock, f.Process)
	if e := s.Claim(context.Background()); e != nil {
		t.Fatal(e)
	}
	deadline := s.deadline
	now = now.Add(5 * time.Second)
	if e := s.Complete(context.Background(), "prepared", "prepared"); e != nil {
		t.Fatal(e)
	}
	if s.deadline.After(deadline) {
		t.Fatal("reply arrival renewed original deadline")
	}
	now = now.Add(5 * time.Second)
	if e := s.Complete(context.Background(), "prepared", "prepared"); e != nil {
		t.Fatal(e)
	}
	if s.deadline.After(deadline) {
		t.Fatal("semantic retry renewed deadline")
	}
	now = deadline
	if s.CanCommit() {
		t.Fatal("equality boundary authorized execution")
	}
}
func TestWorkerControlNetworkWaitConsumesFreshObservationBudget(t *testing.T) {
	f := sf(t)
	now := time.Now()
	s := newSession(Envelope{f.Identity, f.Deliveries["A"]}, func(ctx context.Context, r Request) (Response, error) {
		now = now.Add(2 * time.Millisecond)
		return f.response(r, f.Identity.ExpiresAt-1), nil
	}, func() time.Time { return now }, f.Process)
	if s.Claim(context.Background()) == nil || s.CanPrepare() {
		t.Fatal("late positive response became a claim")
	}
}
func TestWorkerControlUnknownClaimAndCompletionStayCancelled(t *testing.T) {
	for _, unknownClaim := range []bool{true, false} {
		t.Run(map[bool]string{true: "claim", false: "completion"}[unknownClaim], func(t *testing.T) {
			f := sf(t)
			calls := 0
			s := newSession(Envelope{f.Identity, f.Deliveries["A"]}, func(ctx context.Context, r Request) (Response, error) {
				calls++
				if unknownClaim || r.Operation == "complete" {
					return Response{}, errors.New("socket response lost")
				}
				return f.response(r, 1800000001001), nil
			}, time.Now, f.Process)
			if unknownClaim {
				if s.Claim(context.Background()) == nil {
					t.Fatal("unknown claim accepted")
				}
			} else {
				if s.Claim(context.Background()) != nil {
					t.Fatal("claim")
				}
				if s.Complete(context.Background(), "prepared", "prepared") == nil {
					t.Fatal("unknown completion accepted")
				}
			}
			before := calls
			s.Cancel()
			if s.Claim(context.Background()) == nil || s.Complete(context.Background(), "prepared", "prepared") == nil || s.CanPrepare() || s.CanCommit() {
				t.Fatal("cancelled gate reopened")
			}
			_, _ = s.ReadStatus(context.Background())
			if calls != before+1 || s.CanPrepare() || s.CanCommit() {
				t.Fatal("status revived cancelled gate")
			}
		})
	}
}
func TestWorkerControlDurationMathIsBounded(t *testing.T) {
	if d := boundedMillis(MaxInteger); d <= 0 || d/time.Millisecond > time.Duration(int64(^uint64(0)>>1)/int64(time.Millisecond)) {
		t.Fatal("duration overflow")
	}
	if boundedMillis(-1) != 0 {
		t.Fatal("negative budget")
	}
}

func TestWorkerControlStatusReconcilesHistoryWithoutReopeningCancelledGate(t *testing.T) {
	f := sf(t)
	var committed Response
	s := newSession(Envelope{f.Identity, f.Deliveries["A"]}, func(ctx context.Context, r Request) (Response, error) {
		response := f.response(r, 1800000001001)
		if r.Operation == "complete" {
			committed = response
		}
		if r.Operation == "read_status" {
			p := *committed.Receipt
			o := *committed.Observation
			o.PermitUsable = false
			o.ServerTimeMS = 1800000002001
			response.Receipt = &p
			response.Observation = &o
		}
		return response, nil
	}, time.Now, f.Process)
	if s.Claim(context.Background()) != nil || s.Complete(context.Background(), "prepared", "prepared") != nil {
		t.Fatal("intact pipeline")
	}
	s.Cancel()
	response, err := s.ReadStatus(context.Background())
	if err != nil || response.Receipt.State != "commit_authorized" || s.CanPrepare() || s.CanCommit() {
		t.Fatalf("historical reconciliation restored cancelled gate: err=%v", err)
	}
}

func TestWorkerControlDeliveryWallClockSkewCannotLengthenAuthority(t *testing.T) {
	f := sf(t)
	for _, skew := range []time.Duration{-time.Hour, time.Hour} {
		now := time.UnixMilli(1800000000000).Add(skew)
		calls := 0
		s := newSession(Envelope{f.Identity, f.Deliveries["A"]}, func(ctx context.Context, r Request) (Response, error) {
			calls++
			return f.response(r, 1800000000001), nil
		}, func() time.Time { return now }, f.Process)
		err := s.Claim(context.Background())
		if skew < 0 {
			if err != nil || s.deadline.Sub(now) > 30*time.Second {
				t.Fatal("slow wall clock lengthened budget")
			}
		} else if err == nil || calls != 0 {
			t.Fatal("expired delivery reached authority")
		}
	}
}

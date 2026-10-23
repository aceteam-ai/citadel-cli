package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

func controlCenterDetailFixture() status.WorkerDetail {
	poll := time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC)
	pulled := false
	return status.WorkerDetail{
		Mode: "worker", Connected: true, NodeName: "selected-node", MeshIP: "100.64.0.7", Version: "fixture", PID: 123, UptimeSec: 10, ObservedAt: poll,
		Worker: &status.WorkerLiveness{Consuming: true, LastPollAt: &poll, InFlight: 1, Processed: 2, PubSubTransport: "websocket"},
		Swap: &status.SwapActivity{SwapsPerHour: 1, MaxEvictingPerHour: 5, Recent: []status.SwapRecord{{Backend: "ollama", Model: "fixture-model",
			Evicted: []string{"fixture-engine"}, StartedAt: poll, Outcome: "ready", Pulled: &pulled}}},
		Lanes: []status.LaneActivity{{Lane: "inference", Queued: 1, Executing: 1, ExecCapacity: 2, BusySince: &poll}},
	}
}

func controlCenterDetailJSON(t *testing.T, detail status.WorkerDetail) string {
	t.Helper()
	body, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestControlCenterLoadsBoundedWorkerDetailAndClosesResponse(t *testing.T) {
	raw := controlCenterDetailJSON(t, controlCenterDetailFixture())
	body := &sessionViewBody{Reader: strings.NewReader(raw)}
	calls := 0
	detail, err := loadSessionWorkerDetail(context.Background(), "selected-state", func(ctx context.Context, stateDir string) (*http.Response, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 3*time.Second || stateDir != "selected-state" {
			t.Fatal("detail request lost its local destination or deadline")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
	})
	if err != nil || calls != 1 || !body.closed || detail == nil || detail.PID != 123 || detail.Worker == nil || !detail.Worker.Consuming {
		t.Fatalf("bounded detail not projected: detail=%+v err=%v calls=%d closed=%v", detail, err, calls, body.closed)
	}
	for _, extra := range []int{0, 1} {
		bounded := raw + strings.Repeat(" ", status.WorkerDetailMaxBytes-len(raw)+extra)
		responseBody := &sessionViewBody{Reader: strings.NewReader(bounded)}
		got, err := loadSessionWorkerDetail(context.Background(), "fixture", func(context.Context, string) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: responseBody}, nil
		})
		if !responseBody.closed || extra == 0 && (err != nil || got == nil) || extra == 1 && (err != status.ErrWorkerDetailUnavailable || got != nil) {
			t.Fatalf("complete response bound extra=%d: detail=%v err=%v closed=%v", extra, got, err, responseBody.closed)
		}
	}
}

func TestControlCenterWorkerDetailFailuresStayUnavailable(t *testing.T) {
	raw := controlCenterDetailJSON(t, controlCenterDetailFixture())
	for _, invalid := range []string{"", "null", "[]", raw + "{}", "\xff", strings.Replace(raw, `"pid":123`, `"pid":0`, 1),
		strings.Replace(raw, `"pid":123`, `"pid":123,"pid":456`, 1), strings.Replace(raw, `"mode":"worker",`, "", 1)} {
		body := &sessionViewBody{Reader: strings.NewReader(invalid)}
		detail, err := loadSessionWorkerDetail(context.Background(), "fixture", func(context.Context, string) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
		})
		if detail != nil || err != status.ErrWorkerDetailUnavailable || !body.closed {
			t.Fatalf("malformed detail retained observations: detail=%v err=%v closed=%v", detail, err, body.closed)
		}
	}
	for _, code := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusFound, http.StatusServiceUnavailable} {
		body := &sessionViewBody{Reader: strings.NewReader("private-peer-diagnostic")}
		detail, err := loadSessionWorkerDetail(context.Background(), "fixture", func(context.Context, string) (*http.Response, error) {
			return &http.Response{StatusCode: code, Body: body}, nil
		})
		if detail != nil || err != status.ErrWorkerDetailUnavailable || !body.closed || strings.Contains(err.Error(), "private-peer-diagnostic") {
			t.Fatal("HTTP or legacy-route refusal leaked peer text or looked healthy")
		}
	}
	for _, response := range []*http.Response{nil, {StatusCode: http.StatusOK}, {StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(raw))}} {
		detail, err := loadSessionWorkerDetail(context.Background(), "fixture", func(context.Context, string) (*http.Response, error) {
			return response, errors.New("private-transport-diagnostic")
		})
		if detail != nil || err != status.ErrWorkerDetailUnavailable {
			t.Fatal("transport failure was not a static unavailable observation")
		}
	}
	body := &sessionViewBody{Reader: strings.NewReader(raw)}
	_, err := loadSessionWorkerDetail(context.Background(), "fixture", func(context.Context, string) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: body}, errors.New("simultaneous response and error")
	})
	if err != status.ErrWorkerDetailUnavailable || !body.closed {
		t.Fatal("error plus response leaked its body")
	}
}

func TestControlCenterWorkerDetailFutureFieldsAndCancellation(t *testing.T) {
	raw := controlCenterDetailJSON(t, controlCenterDetailFixture())
	additive := strings.TrimSuffix(raw, "}") + `,"future":{"optional":true}}`
	detail, err := loadSessionWorkerDetail(context.Background(), "fixture", func(context.Context, string) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(additive))}, nil
	})
	if err != nil || detail == nil || detail.PID != 123 {
		t.Fatal("bounded additive response broke the known projection", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	body := &sessionViewBody{Reader: strings.NewReader(raw)}
	detail, err = loadSessionWorkerDetail(ctx, "fixture", func(context.Context, string) (*http.Response, error) {
		cancel()
		return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
	})
	if detail != nil || err != status.ErrWorkerDetailUnavailable || !body.closed {
		t.Fatal("cancelled request published a completed-but-stale response")
	}
}

func TestControlCenterWorkerObserverPinsOwnerWithoutAdoptingReplacement(t *testing.T) {
	first := controlCenterDetailFixture()
	for _, change := range []func(*status.WorkerDetail){
		func(detail *status.WorkerDetail) { detail.PID++ },
		func(detail *status.WorkerDetail) { detail.Mode = "presence" },
		func(detail *status.WorkerDetail) { detail.Version = "replacement" },
		func(detail *status.WorkerDetail) { detail.NodeName = "replacement" },
		func(detail *status.WorkerDetail) { detail.UptimeSec-- },
	} {
		observer := sessionWorkerDetailObserver{pid: first.PID}
		if !observer.accept(&first) {
			t.Fatal("selected owner refused")
		}
		replacement := first
		change(&replacement)
		if observer.accept(&replacement) || !observer.changed || observer.accept(&first) {
			t.Fatal("changed owner was adopted or unlatched without explicit restart")
		}
	}
	for _, pid := range []int{0, -1, first.PID + 1} {
		observer := sessionWorkerDetailObserver{pid: pid}
		if observer.accept(&first) || !observer.changed {
			t.Fatal("missing or different selected owner silently adopted")
		}
	}
	observer := sessionWorkerDetailObserver{pid: first.PID}
	if !observer.accept(&first) {
		t.Fatal("first observation refused")
	}
	reconnected := first
	reconnected.MeshIP, reconnected.Connected, reconnected.UptimeSec = "100.64.0.8", false, first.UptimeSec+1
	if !observer.accept(&reconnected) {
		t.Fatal("same-owner reconnect became an owner replacement")
	}
}

func TestControlCenterWorkerMonitorClearsAndRecoversSameOwnerSequentially(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := controlCenterDetailFixture()
	newer := first
	newer.UptimeSec++
	raws := []string{controlCenterDetailJSON(t, first), "", controlCenterDetailJSON(t, newer)}
	calls, waits := 0, 0
	inRequest := false
	bodies := []*sessionViewBody{}
	request := func(context.Context, string) (*http.Response, error) {
		if inRequest || calls >= len(raws) {
			t.Fatal("monitor overlapped requests or ran beyond cancellation")
		}
		inRequest = true
		defer func() { inRequest = false }()
		raw := raws[calls]
		calls++
		if raw == "" {
			return nil, errors.New("offline")
		}
		body := &sessionViewBody{Reader: strings.NewReader(raw)}
		bodies = append(bodies, body)
		return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
	}
	var snapshots []*status.WorkerDetail
	var receipts []time.Time
	observedAt := first.ObservedAt.Add(time.Second)
	publish := func(detail *status.WorkerDetail, receipt time.Time) {
		snapshots = append(snapshots, detail)
		receipts = append(receipts, receipt)
	}
	wait := func(context.Context) bool {
		if inRequest {
			t.Fatal("monitor waited before request completion")
		}
		waits++
		if waits == len(raws) {
			cancel()
			return false
		}
		return true
	}
	err := observeControlCenterWorker(ctx, "fixture", first.PID, request, publish, wait, func() time.Time { return observedAt })
	if err != context.Canceled || calls != 3 || len(snapshots) != 5 || snapshots[0] != nil || snapshots[1] == nil || snapshots[2] != nil || snapshots[3] == nil || snapshots[4] != nil {
		t.Fatalf("monitor did not clear then recover the same owner: err=%v calls=%d snapshots=%v", err, calls, snapshots)
	}
	if snapshots[3].UptimeSec != newer.UptimeSec || !receipts[1].Equal(observedAt) || !receipts[2].IsZero() || !receipts[4].IsZero() {
		t.Fatal("freshness or unavailable state retained an older receipt")
	}
	for _, body := range bodies {
		if !body.closed {
			t.Fatal("monitor leaked a response body")
		}
	}
}

func TestControlCenterWorkerMonitorStopsReadingChangedOwner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := controlCenterDetailFixture()
	replacement := first
	replacement.PID++
	calls, waits := 0, 0
	var last *status.WorkerDetail
	request := func(context.Context, string) (*http.Response, error) {
		calls++
		detail := first
		if calls > 1 {
			detail = replacement
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(controlCenterDetailJSON(t, detail)))}, nil
	}
	wait := func(context.Context) bool {
		waits++
		if waits == 3 {
			cancel()
			return false
		}
		return true
	}
	err := observeControlCenterWorker(ctx, "fixture", first.PID, request, func(detail *status.WorkerDetail, _ time.Time) { last = detail }, wait, time.Now)
	if err != context.Canceled || calls != 2 || last != nil {
		t.Fatalf("replacement kept being polled/adopted: err=%v calls=%d last=%v", err, calls, last)
	}
}

func TestControlCenterWorkerDetailSnapshotsHaveIndependentOwnership(t *testing.T) {
	defer resetControlCenterWorkerDetail()
	detail := controlCenterDetailFixture()
	receipt := detail.ObservedAt.Add(time.Second)
	ccWorkerMu.Lock()
	ccSharedWorkerMonitor, ccSharedWorkerDetail, ccSharedWorkerObservedAt = true, &detail, receipt
	ccWorkerMu.Unlock()
	monitor, snapshot, observedAt := controlCenterWorkerDetailSnapshot()
	if !monitor || snapshot == nil || !observedAt.Equal(receipt) {
		t.Fatal("monitor snapshot unavailable")
	}
	snapshot.Worker.Consuming = false
	*snapshot.Worker.LastPollAt = time.Time{}
	snapshot.Swap.Recent[0].Evicted[0] = "mutated"
	*snapshot.Swap.Recent[0].Pulled = true
	*snapshot.Lanes[0].BusySince = time.Time{}
	_, fresh, _ := controlCenterWorkerDetailSnapshot()
	if fresh == nil || !fresh.Worker.Consuming || fresh.Worker.LastPollAt.IsZero() || fresh.Swap.Recent[0].Evicted[0] == "mutated" || *fresh.Swap.Recent[0].Pulled || fresh.Lanes[0].BusySince.IsZero() {
		t.Fatal("consumer mutated monitor-owned nested observations")
	}
	resetControlCenterWorkerDetail()
	monitor, snapshot, observedAt = controlCenterWorkerDetailSnapshot()
	if monitor || snapshot != nil || !observedAt.IsZero() {
		t.Fatal("monitor exit retained stale state")
	}
}

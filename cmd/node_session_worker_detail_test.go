package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nodesession"
	"github.com/aceteam-ai/citadel-cli/internal/status"
)

func TestNodeSessionWorkerDetailUsesLiveProvidersWithoutCollector(t *testing.T) {
	started := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	now := started.Add(10 * time.Second)
	pulled := false
	live := status.WorkerLiveness{Consuming: true, Processed: 1}
	swap := status.SwapActivity{Recent: []status.SwapRecord{{Backend: "ollama", Pulled: &pulled}}}
	lanes := []status.LaneActivity{{Lane: "inference", Queued: 1}}
	calls := [3]int{}
	handler := nodeSessionWorkerDetailHandler(nodesession.Worker, func(context.Context) nodeSessionStatus {
		return nodeSessionStatus{Connected: true, NodeName: "selected", MeshIP: "100.64.0.9"}
	}, nodeSessionWorkerProviders{
		Worker: func() *status.WorkerLiveness { calls[0]++; return &live },
		Swap:   func() *status.SwapActivity { calls[1]++; return &swap },
		Lanes:  func() []status.LaneActivity { calls[2]++; return lanes },
	}, started, func() time.Time { return now })
	read := func() status.WorkerDetail {
		t.Helper()
		response := httptest.NewRecorder()
		handler(response, httptest.NewRequest(http.MethodGet, sessionWorkerDetailRoute, nil))
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("detail response: %d %s", response.Code, response.Body.String())
		}
		got, err := status.DecodeWorkerDetail(response.Body.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	first := read()
	if first.Mode != "worker" || first.PID != os.Getpid() || first.Version != Version || first.NodeName != "selected" || first.UptimeSec != 10 || !first.ObservedAt.Equal(now) || !first.Worker.Consuming || *first.Swap.Recent[0].Pulled || first.Lanes[0].Queued != 1 {
		t.Fatalf("provider projection changed: %+v", first)
	}
	live.Consuming, live.Processed = false, 2
	pulled = true
	lanes[0].Queued = 3
	now = now.Add(time.Second)
	second := read()
	if second.Worker.Consuming || second.Worker.Processed != 2 || !*second.Swap.Recent[0].Pulled || second.Lanes[0].Queued != 3 || second.UptimeSec != 11 || calls != [3]int{2, 2, 2} {
		t.Fatal("detail reused stale provider values", second, calls)
	}
	if !first.Worker.Consuming || *first.Swap.Recent[0].Pulled || first.Lanes[0].Queued != 1 {
		t.Fatal("delivered observation aliases later provider changes")
	}
}

func TestNodeSessionWorkerDetailMissingAndUninitializedProvidersAreUnknown(t *testing.T) {
	for _, providers := range []nodeSessionWorkerProviders{
		{},
		{Worker: func() *status.WorkerLiveness { return nil }, Swap: func() *status.SwapActivity { return nil }, Lanes: func() []status.LaneActivity { return nil }},
	} {
		now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
		handler := nodeSessionWorkerDetailHandler(nodesession.Presence, func(context.Context) nodeSessionStatus { return nodeSessionStatus{} }, providers, now, func() time.Time { return now })
		response := httptest.NewRecorder()
		handler(response, httptest.NewRequest(http.MethodGet, sessionWorkerDetailRoute, nil))
		detail, err := status.DecodeWorkerDetail(response.Body.Bytes())
		if response.Code != http.StatusOK || err != nil || detail.Worker != nil || detail.Swap != nil || detail.Lanes != nil {
			t.Fatal("unknown provider became a healthy zero", response.Body.String(), err)
		}
	}
}

func TestNodeSessionWorkerDetailOverflowRefusesWholeReply(t *testing.T) {
	for _, oversizedReply := range []bool{false, true} {
		swap := status.SwapActivity{Recent: []status.SwapRecord{{Model: strings.Repeat("private-model", 30)}}}
		if oversizedReply {
			swap.Recent = make([]status.SwapRecord, 64)
			for i := range swap.Recent {
				swap.Recent[i].Evicted = make([]string, 64)
				for j := range swap.Recent[i].Evicted {
					swap.Recent[i].Evicted[j] = strings.Repeat("x", 256)
				}
			}
		}
		now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
		handler := nodeSessionWorkerDetailHandler(nodesession.Worker, func(context.Context) nodeSessionStatus { return nodeSessionStatus{} }, nodeSessionWorkerProviders{Swap: func() *status.SwapActivity { return &swap }}, now, func() time.Time { return now })
		response := httptest.NewRecorder()
		handler(response, httptest.NewRequest(http.MethodGet, sessionWorkerDetailRoute, nil))
		if response.Code != http.StatusServiceUnavailable || response.Body.String() != `{"error":"worker detail unavailable"}` {
			t.Fatalf("overflow emitted partial data or provider details: %d %s", response.Code, response.Body.String())
		}
	}
}

func TestWorkerDetailDoesNotChangeDefaultStatusDTO(t *testing.T) {
	raw, err := json.Marshal(nodeSessionStatus{Mode: nodesession.Worker, Version: "legacy", PID: 42})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"mode":"worker","connected":false,"version":"legacy","pid":42,"uptime_seconds":0}` {
		t.Fatal("opt-in worker detail changed the existing status contract", string(raw))
	}
}

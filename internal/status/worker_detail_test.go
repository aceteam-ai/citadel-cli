package status

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func workerDetailFixture() WorkerDetail {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	pulled := false
	return WorkerDetail{
		Mode:       "worker",
		Connected:  true,
		NodeName:   "node",
		MeshIP:     "100.64.0.2",
		Version:    "test",
		PID:        42,
		UptimeSec:  10,
		ObservedAt: now,
		Worker:     &WorkerLiveness{Consuming: true, InFlight: 1, Processed: 2, Failed: 3, LastPollAt: &now, LastJobConsumedAt: &now, PubSubTransport: "websocket"},
		Swap:       &SwapActivity{SwapsPerHour: 1, MaxEvictingPerHour: 4, Recent: []SwapRecord{{Backend: "ollama", Model: "model", Evicted: []string{"vllm"}, StartedAt: now, Wait: time.Second, Outcome: "ready", Pulled: &pulled}}},
		Lanes:      []LaneActivity{{Lane: "inference", Queued: 1, Executing: 1, ExecCapacity: 2, BusySince: &now}},
	}
}

func TestWorkerDetailCopiesNestedProviderValues(t *testing.T) {
	original := workerDetailFixture()
	cloned, err := CloneWorkerDetail(original)
	if err != nil {
		t.Fatal(err)
	}
	cloned.Worker.InFlight = 99
	*cloned.Worker.LastPollAt = time.Time{}
	*cloned.Worker.LastJobConsumedAt = time.Time{}
	cloned.Swap.Recent[0].Evicted[0] = "changed"
	*cloned.Swap.Recent[0].Pulled = true
	cloned.Swap.Recent[0].Backend = "changed"
	*cloned.Lanes[0].BusySince = time.Time{}
	cloned.Lanes[0].Queued = 99
	if original.Worker.InFlight != 1 || original.Worker.LastPollAt.IsZero() || original.Worker.LastJobConsumedAt.IsZero() ||
		original.Swap.Recent[0].Evicted[0] != "vllm" || *original.Swap.Recent[0].Pulled || original.Swap.Recent[0].Backend != "ollama" ||
		original.Lanes[0].BusySince.IsZero() || original.Lanes[0].Queued != 1 {
		t.Fatal("clone aliases a provider pointer or slice")
	}
	for _, lanes := range [][]LaneActivity{nil, {}} {
		original.Lanes = lanes
		cloned, err := CloneWorkerDetail(original)
		if err != nil || (cloned.Lanes == nil) != (lanes == nil) {
			t.Fatal("clone lost unknown versus initialized-empty lanes", err)
		}
	}
}

func TestWorkerDetailPreservesPulledTriStateAndUnknownProviders(t *testing.T) {
	for _, state := range []string{"unknown", "cached", "fetched"} {
		t.Run(state, func(t *testing.T) {
			detail := workerDetailFixture()
			if state == "unknown" {
				detail.Swap.Recent[0].Pulled = nil
			} else {
				pulled := state == "fetched"
				detail.Swap.Recent[0].Pulled = &pulled
			}
			raw, err := EncodeWorkerDetail(detail)
			if err != nil {
				t.Fatal(err)
			}
			got, err := DecodeWorkerDetail(raw)
			if err != nil {
				t.Fatal(err)
			}
			pulled := got.Swap.Recent[0].Pulled
			if state == "unknown" && pulled != nil || state != "unknown" && (pulled == nil || *pulled != (state == "fetched")) {
				t.Fatal("unknown/cached/fetched value changed")
			}
		})
	}
	detail := workerDetailFixture()
	detail.Worker, detail.Swap, detail.Lanes = nil, nil, nil
	raw, err := EncodeWorkerDetail(detail)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeWorkerDetail(raw)
	if err != nil || got.Worker != nil || got.Swap != nil || got.Lanes != nil {
		t.Fatal("unknown providers became healthy zero values", err)
	}
}

func TestWorkerDetailSourceLimitsRefuseWithoutTruncation(t *testing.T) {
	for _, change := range []struct {
		name string
		edit func(*WorkerDetail)
	}{
		{"long node", func(d *WorkerDetail) { d.NodeName = strings.Repeat("x", 257) }},
		{"invalid UTF8", func(d *WorkerDetail) { d.NodeName = "\xff" }},
		{"long transport", func(d *WorkerDetail) { d.Worker.PubSubTransport = strings.Repeat("x", 257) }},
		{"long model", func(d *WorkerDetail) { d.Swap.Recent[0].Model = strings.Repeat("x", 257) }},
		{"long backend", func(d *WorkerDetail) { d.Swap.Recent[0].Backend = strings.Repeat("x", 257) }},
		{"long evicted", func(d *WorkerDetail) { d.Swap.Recent[0].Evicted[0] = strings.Repeat("x", 257) }},
		{"too many records", func(d *WorkerDetail) { d.Swap.Recent = make([]SwapRecord, 65) }},
		{"too many evictions", func(d *WorkerDetail) { d.Swap.Recent[0].Evicted = make([]string, 65) }},
		{"too many lanes", func(d *WorkerDetail) { d.Lanes = make([]LaneActivity, 17) }},
		{"negative counter", func(d *WorkerDetail) { d.Worker.InFlight = -1 }},
	} {
		t.Run(change.name, func(t *testing.T) {
			detail := workerDetailFixture()
			change.edit(&detail)
			if raw, err := EncodeWorkerDetail(detail); err != ErrWorkerDetailUnavailable || raw != nil {
				t.Fatalf("invalid detail was encoded or truncated: %s %v", raw, err)
			}
		})
	}
	detail := workerDetailFixture()
	detail.Swap.Recent[0].Model = strings.Repeat("é", 128) // Exactly 256 UTF-8 bytes.
	detail.Swap.Recent[0].Evicted = make([]string, 64)
	detail.Swap.Recent = append(make([]SwapRecord, 63), detail.Swap.Recent[0])
	detail.Lanes = make([]LaneActivity, 16)
	for i := range detail.Lanes {
		detail.Lanes[i] = LaneActivity{Lane: "inference"}
	}
	if _, err := EncodeWorkerDetail(detail); err != nil {
		t.Fatal("inclusive collection/string limits refused", err)
	}
	// Individual limits pass, but the complete encoded reply is still bounded.
	for i := range detail.Swap.Recent {
		detail.Swap.Recent[i].Evicted = make([]string, 64)
		for j := range detail.Swap.Recent[i].Evicted {
			detail.Swap.Recent[i].Evicted[j] = strings.Repeat("x", 256)
		}
	}
	if raw, err := EncodeWorkerDetail(detail); err != ErrWorkerDetailUnavailable || raw != nil {
		t.Fatal("complete reply exceeded its cap", err)
	}
}

func TestWorkerDetailDecoderRejectsMalformedKnownFieldsAndDuplicates(t *testing.T) {
	raw, err := EncodeWorkerDetail(workerDetailFixture())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{
		[]byte(`null`), []byte(`[]`), append(append([]byte{}, raw...), []byte(` {}`)...),
		bytes.Replace(raw, []byte(`"pid":42`), []byte(`"pid":42,"pid":43`), 1),
		bytes.Replace(raw, []byte(`"pid":42`), []byte(`"PID":42`), 1),
		bytes.Replace(raw, []byte(`"pid":42`), []byte(`"pid":null`), 1),
		bytes.Replace(raw, []byte(`"consuming":true`), []byte(`"consuming":true,"consuming":false`), 1),
		bytes.Replace(raw, []byte(`"consuming":true,`), nil, 1),
		bytes.Replace(raw, []byte(`"swap":{`), []byte(`"swap":{"future":{"a":1,"a":2},`), 1),
		bytes.Replace(raw, []byte(`"backend":"ollama"`), []byte(`"backend":null`), 1),
		bytes.Replace(raw, []byte(`"evicted":["vllm"]`), []byte(`"evicted":[null]`), 1),
		bytes.Replace(raw, []byte(`"queued":1`), []byte(`"queued":null`), 1),
		bytes.Replace(raw, []byte(`"node_name":"node"`), []byte(`"node_name":"`+strings.Repeat("x", 257)+`"`), 1),
		append([]byte{}, 0xff),
	} {
		if bytes.Equal(raw, bad) {
			t.Fatal("malformed fixture did not change its target field")
		}
		if _, err := DecodeWorkerDetail(bad); err != ErrWorkerDetailUnavailable {
			t.Fatalf("malformed detail accepted: %s %v", bad, err)
		}
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		t.Fatal("fixture is not an object")
	}
	fields["future"] = json.RawMessage(`{"nested":[1,true,"additive"]}`)
	additive, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeWorkerDetail(additive); err != nil {
		t.Fatal("bounded additive field refused", err)
	}
	padded := append(append([]byte{}, raw...), bytes.Repeat([]byte{' '}, WorkerDetailMaxBytes-len(raw))...)
	if _, err := DecodeWorkerDetail(padded); err != nil {
		t.Fatal("exact complete response bound refused", err)
	}
	if _, err := DecodeWorkerDetail(append(padded, ' ')); err != ErrWorkerDetailUnavailable {
		t.Fatal("complete response bound was extended", err)
	}
}

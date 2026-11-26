package harnessdriver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func sampleEvent(seq uint64) Event {
	return Event{Protocol: Version, Type: "assistant.delta", SessionName: "synthetic", Incarnation: "generation-1", NativeID: "", TurnID: "turn-1", Seq: seq, TS: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Source: "native", Fidelity: "structured", Data: json.RawMessage(`{"text":"hello"}`)}
}

func TestProtocolRequiresTypedBoundedEnvelope(t *testing.T) {
	e := sampleEvent(1)
	raw, _ := json.Marshal(e)
	if _, err := DecodeEvent(raw); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Event){
		func(e *Event) { e.Protocol = "other" }, func(e *Event) { e.Seq = 0 }, func(e *Event) { e.Source = "guessed" },
		func(e *Event) { e.Type = "new.unreviewed" }, func(e *Event) { e.Incarnation = "" },
		func(e *Event) { e.Data = json.RawMessage(`{"text":"hello","unreviewed":true}`) },
		func(e *Event) { e.Data = json.RawMessage(`{"text":"hello","text":"other"}`) },
	} {
		bad := e
		change(&bad)
		raw, _ := json.Marshal(bad)
		if _, err := DecodeEvent(raw); !errors.Is(err, ErrProtocol) {
			t.Fatalf("accepted invalid event: %s (%v)", raw, err)
		}
	}
	bad := strings.Replace(string(raw), `"seq":1`, `"seq":1,"seq":2`, 1)
	if _, err := DecodeEvent([]byte(bad)); !errors.Is(err, ErrProtocol) {
		t.Fatal("duplicate envelope accepted")
	}
	if _, err := DecodeEvent([]byte(`{"private":"peer contents"`)); err == nil || strings.Contains(err.Error(), "peer") {
		t.Fatal(err)
	}
}

func TestQueueBackpressureNeverDropsControlAndCopiesData(t *testing.T) {
	q, err := NewEventQueue(1)
	if err != nil {
		t.Fatal(err)
	}
	first := sampleEvent(1)
	if err := q.Push(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	first.Data[0] = '!'
	second := sampleEvent(2)
	second.Type = "turn.completed"
	second.Data = json.RawMessage(`{"outcome":"failed","tool_errors":1,"denials":0}`)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- q.Push(ctx, second) }()
	select {
	case err := <-done:
		t.Fatalf("full queue did not backpressure: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	got, err := q.Pop(context.Background())
	if err != nil || got.Seq != 1 || got.Data[0] != '{' {
		t.Fatalf("queue alias/order: %+v %v", got, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("producer stuck")
	}
	q.Close()
	got, err = q.Pop(context.Background())
	if err != nil || got.Type != "turn.completed" {
		t.Fatalf("control lost: %+v %v", got, err)
	}
	if _, err = q.Pop(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if err = q.Push(context.Background(), sampleEvent(3)); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	cancel()
}

func TestQueueCancellationUnblocksFullProducer(t *testing.T) {
	q, _ := NewEventQueue(1)
	_ = q.Push(context.Background(), sampleEvent(1))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := q.Push(ctx, sampleEvent(2)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	q.Close()
}

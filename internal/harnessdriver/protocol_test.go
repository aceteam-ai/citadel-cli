package harnessdriver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
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
		badRaw, _ := json.Marshal(bad)
		if _, err := DecodeEvent(badRaw); !errors.Is(err, ErrProtocol) {
			t.Fatalf("accepted invalid event: %s (%v)", badRaw, err)
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

func TestGoldenCommandsAndEventsShareSchema(t *testing.T) {
	raw, err := os.ReadFile("testdata/protocol.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	r, _ := NewLineReader(strings.NewReader(string(raw)), MaxLineBytes)
	types := map[string]bool{}
	for {
		line, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var tag struct {
			Type string `json:"type"`
		}
		if err = json.Unmarshal(line, &tag); err != nil {
			t.Fatal(err)
		}
		var again []byte
		if strings.HasPrefix(tag.Type, "session.") && tag.Type != "session.error" || tag.Type == "turn.submit" || tag.Type == "approval.respond" || tag.Type == "turn.interrupt" {
			c, err := DecodeCommand(line)
			if err != nil {
				t.Fatalf("%s: %v", tag.Type, err)
			}
			again, err = json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
		} else {
			e, err := DecodeEvent(line)
			if err != nil {
				t.Fatalf("%s: %v", tag.Type, err)
			}
			again, err = json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
		}
		var before, after any
		_ = json.Unmarshal(line, &before)
		_ = json.Unmarshal(again, &after)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("golden changed: %s", tag.Type)
		}
		types[tag.Type] = true
	}
	if len(types) != 16 {
		t.Fatalf("missing vocabulary fixtures: %v", types)
	}
}

func TestSchemaPayloadBoundaries(t *testing.T) {
	for _, tc := range []struct {
		kind      string
		good, bad string
	}{
		{"assistant.text", `{"text":""}`, `{"text":null}`},
		{"tool.activity", `{"id":"tool-1","state":"failed","exit_code":2,"is_error":true,"input_preview":"","output_excerpt":""}`, `{"id":"tool-1","state":"started","exit_code":2,"is_error":false,"input_preview":"","output_excerpt":""}`},
		{"approval.requested", `{"id":"approval-1","kind":"command","choices":["decline"]}`, `{"id":"approval-1","kind":"command","choices":["decline","decline"]}`},
		{"approval.resolved", `{"id":"approval-1","decision":"decline"}`, `{"id":"approval-1","decision":"accept_always"}`},
		{"usage.reported", `{"model":"","input_tokens":null,"output_tokens":0,"cached_tokens":null}`, `{"model":"","input_tokens":9007199254740992,"output_tokens":0,"cached_tokens":0}`},
		{"quota.reported", `{"window":"unknown","used_percent":null,"resets_at":null}`, `{"window":"five_hour","used_percent":101,"resets_at":null}`},
		{"models.reported", `{"ids":["synthetic-model"]}`, `{"ids":["synthetic-model","synthetic-model"]}`},
		{"turn.completed", `{"outcome":"unknown","tool_errors":1,"denials":1}`, `{"outcome":"completed","tool_errors":-1,"denials":0}`},
		{"session.error", `{"code":"native_error","retryable":false}`, `{"code":"peer secret diagnostic","retryable":true}`},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			e := sampleEvent(1)
			e.Type = tc.kind
			e.Data = json.RawMessage(tc.good)
			if err := e.Validate(); err != nil {
				t.Fatal(err)
			}
			e.Data = json.RawMessage(tc.bad)
			if err := e.Validate(); !errors.Is(err, ErrProtocol) {
				t.Fatal("bad payload accepted")
			}
		})
	}
	e := sampleEvent(1)
	e.Seq = 9007199254740992
	if err := e.Validate(); !errors.Is(err, ErrProtocol) {
		t.Fatal("unsafe sequence accepted")
	}
	e = sampleEvent(1)
	e.Data = json.RawMessage(`{"text":"` + strings.Repeat("a", 32769) + `"}`)
	if err := e.Validate(); !errors.Is(err, ErrProtocol) {
		t.Fatal("text bound bypassed")
	}
	for _, raw := range []string{strings.Repeat(" ", MaxEnvelopeBytes+1), `{"x":` + strings.Repeat("[", 18) + `0` + strings.Repeat("]", 18) + `}`, `null`, `{}`} {
		if _, err := DecodeEvent([]byte(raw)); !errors.Is(err, ErrProtocol) {
			t.Fatal("bad envelope accepted")
		}
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

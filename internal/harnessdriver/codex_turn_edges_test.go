package harnessdriver

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func commandNotification(id, status string, exit any) []byte {
	raw, _ := json.Marshal(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "item": map[string]any{"type": "commandExecution", "id": id, "status": status, "exitCode": exit}}})
	return raw
}

func TestCodexOfflineCommandStatesAndUnknownExit(t *testing.T) {
	r := offlineTurn(t)
	startOfflineTurn(t, r)
	for _, tc := range []struct {
		id, status              string
		exit                    any
		want                    string
		wantErrors, wantDenials uint64
	}{
		{"command-1", "completed", nil, "completed", 0, 0},
		{"command-2", "completed", 2, "failed", 1, 0},
		{"command-3", "declined", nil, "failed", 1, 1},
		{"command-4", "failed", nil, "failed", 2, 1},
	} {
		got, err := r.Apply(commandNotification(tc.id, tc.status, tc.exit), projectionTime)
		if err != nil {
			t.Fatal(err)
		}
		var tool Tool
		_ = json.Unmarshal(got.Events[0].Data, &tool)
		if tool.State != tc.want || (tc.exit == nil && tool.ExitCode != nil) || r.toolErrors != tc.wantErrors || r.denials != tc.wantDenials {
			t.Fatalf("bad state/count: %+v", tool)
		}
		for _, e := range got.Events {
			if err := e.Validate(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestCodexOfflineTextItemSnapshotsAndReplay(t *testing.T) {
	r := offlineTurn(t)
	startOfflineTurn(t, r)
	delta := []byte(`{"method":"item/agentMessage/delta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"message-1","delta":"partial"}}`)
	snapshot := []byte(`{"method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","item":{"type":"agentMessage","id":"message-1","text":"final"}}}`)
	for i, raw := range [][]byte{delta, snapshot} {
		got, err := r.Apply(raw, projectionTime)
		if err != nil {
			t.Fatal(err)
		}
		var text Text
		_ = json.Unmarshal(got.Events[0].Data, &text)
		if text.ItemID != "message-1" || got.Events[0].Seq != uint64(i+1) {
			t.Fatal("text item correlation lost")
		}
		if i == 1 && (text.Text != "final" || got.Events[0].Type != "assistant.text") {
			t.Fatal("snapshot treated as append")
		}
		got.Events[0].Data[0] = '!'
	}
	if got, err := r.Apply(snapshot, projectionTime); err != nil || got.Kind != "duplicate" {
		t.Fatal("caller mutation changed replay state")
	}
	conflict := []byte(strings.Replace(string(snapshot), `"text":"final"`, `"text":"changed"`, 1))
	if _, err := r.Apply(conflict, projectionTime); !errors.Is(err, ErrCodexReplay) {
		t.Fatal("conflicting final snapshot accepted")
	}
	if _, err := r.Apply(delta, projectionTime); !errors.Is(err, ErrCodexReplay) {
		t.Fatal("delta after completed item accepted")
	}
	if r.next != 3 {
		t.Fatal("refusal advanced sequence")
	}
}

func TestCodexOfflineObservedCountersConcurrentAndBounded(t *testing.T) {
	r := offlineTurn(t)
	startOfflineTurn(t, r)
	var wg sync.WaitGroup
	seqs := make(chan uint64, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := r.Apply(commandNotification(fmt.Sprintf("command-%d", i), "failed", 2), projectionTime)
			if err != nil {
				t.Error(err)
				return
			}
			seqs <- got.Events[0].Seq
		}(i)
	}
	wg.Wait()
	close(seqs)
	seen := map[uint64]bool{}
	for seq := range seqs {
		if seen[seq] {
			t.Fatal("duplicate sequence")
		}
		seen[seq] = true
	}
	if len(seen) != 50 || r.toolErrors != 50 {
		t.Fatal("concurrent counters lost")
	}
	for i := 50; i < maxCodexItems; i++ {
		if _, err := r.Apply(commandNotification(fmt.Sprintf("command-%d", i), "failed", 2), projectionTime); err != nil {
			t.Fatal(err)
		}
	}
	before := r.next
	if _, err := r.Apply(commandNotification("extra", "failed", 2), projectionTime); !errors.Is(err, ErrCodexProjectionLimit) {
		t.Fatal("unbounded terminal items")
	}
	if r.next != before || r.toolErrors != maxCodexItems {
		t.Fatal("limit mutated counters")
	}
	if got, err := r.Apply(commandNotification("command-0", "failed", 2), projectionTime); err != nil || got.Kind != "duplicate" {
		t.Fatal("bounded replay lost")
	}
}

func TestCodexOfflineSequenceExhaustionDoesNotWrap(t *testing.T) {
	r, err := NewCodexTurn(CodexTurnIdentity{SessionName: "synthetic", Incarnation: "generation-1", ThreadID: "thread-1", TurnID: "turn-1", FirstSeq: maxProjectionSeq})
	if err != nil {
		t.Fatal(err)
	}
	startOfflineTurn(t, r)
	raw := commandNotification("command-1", "failed", 2)
	if got, err := r.Apply(raw, projectionTime); err != nil || got.Events[0].Seq != maxProjectionSeq {
		t.Fatal(err)
	}
	if got, err := r.Apply(raw, projectionTime); err != nil || got.Kind != "duplicate" {
		t.Fatal("exhausted replay not recognized")
	}
	if _, err := r.Apply(commandNotification("command-2", "completed", 0), projectionTime); !errors.Is(err, ErrCodexProjectionLimit) {
		t.Fatal("sequence wrapped")
	}
	if r.toolErrors != 1 || len(r.seen) != 1 {
		t.Fatal("exhaustion partially committed")
	}
}

func TestCodexOfflineLargePrivateOptionalDataIsNotPublished(t *testing.T) {
	r := offlineTurn(t)
	startOfflineTurn(t, r)
	private := strings.Repeat("p", 5<<20)
	raw := []byte(`{"method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","item":{"type":"commandExecution","id":"command-1","status":"failed","exitCode":2,"aggregatedOutput":"` + private + `"}}}`)
	line, _ := NewLineReader(strings.NewReader(string(raw)+"\n"), MaxLineBytes)
	native, err := line.Next()
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Apply(native, projectionTime)
	if err != nil {
		t.Fatal(err)
	}
	projected, _ := json.Marshal(got.Events)
	if len(projected) > 1024 || strings.Contains(string(projected), "pppppppp") {
		t.Fatal("private optional output published")
	}
	for _, value := range []any{r, r.identity, got} {
		if out := fmt.Sprintf("%v %#v", value, value); strings.Contains(out, "thread-1") || strings.Contains(out, "pppppppp") {
			t.Fatal("debug formatting retained content")
		}
	}
	if out, _ := json.Marshal(r); string(out) != "{}" {
		t.Fatal("reducer serialized")
	}
	if out, _ := json.Marshal(r.identity); string(out) != "{}" {
		t.Fatal("identity serialized")
	}
}

func TestCodexOfflineBadTimeAndLimitsHaveNoPartialResult(t *testing.T) {
	r := offlineTurn(t)
	startOfflineTurn(t, r)
	for _, at := range []time.Time{{}, projectionTime.Add(-time.Second), time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)} {
		if _, err := r.Apply(commandNotification("command-1", "failed", 2), at); !errors.Is(err, ErrCodexTurnState) {
			t.Fatal(err)
		}
	}
	deep := []byte(`{"method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","item":{"type":"commandExecution","id":"command-1","status":"failed","exitCode":2,"optional":` + strings.Repeat("[", 18) + `0` + strings.Repeat("]", 18) + `}}}`)
	for _, raw := range [][]byte{deep, append([]byte{0xff}, commandNotification("command-1", "failed", 2)...), []byte(strings.Repeat(" ", MaxLineBytes+1)), []byte(`null`), []byte(`{"method":"item/completed","params":null}`), []byte(`{"method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","item":{"type":"commandExecution","id":"command-1","status":"other","exitCode":0}}}`)} {
		if got, err := r.Apply(raw, projectionTime); err == nil || len(got.Events) != 0 {
			t.Fatal("invalid native input accepted")
		}
	}
	text := `{"method":"item/agentMessage/delta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"message-1","delta":"` + strings.Repeat("x", 32769) + `"}}`
	if _, err := r.Apply([]byte(text), projectionTime); !errors.Is(err, ErrCodexProjectionLimit) {
		t.Fatal("oversize text silently clipped")
	}
	if r.next != 1 || len(r.seen) != 0 {
		t.Fatal("invalid input mutated reducer")
	}
}

func TestCodexOfflineCounterScopeCannotClaimCompleteness(t *testing.T) {
	e := sampleEvent(1)
	e.Type = "turn.completed"
	e.Data = json.RawMessage(`{"outcome":"completed","tool_errors":0,"denials":0,"counts_scope":"complete"}`)
	if err := e.Validate(); !errors.Is(err, ErrProtocol) {
		t.Fatal("invented complete-stream evidence accepted")
	}
}

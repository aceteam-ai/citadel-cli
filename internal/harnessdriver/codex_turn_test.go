package harnessdriver

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var projectionTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func offlineTurn(t *testing.T) *CodexTurn {
	t.Helper()
	r, err := NewCodexTurn(CodexTurnIdentity{SessionName: "synthetic", Incarnation: "generation-1", ThreadID: "thread-1", TurnID: "turn-1", FirstSeq: 1})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func startOfflineTurn(t *testing.T, r *CodexTurn) {
	t.Helper()
	got, err := r.Apply([]byte(`{"method":"turn/started","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"inProgress"}}}`), projectionTime)
	if err != nil || got.Kind != "turn_started" || len(got.Events) != 0 {
		t.Fatalf("start: %+v %v", got, err)
	}
}

func TestCodexOfflineFailureIsIndependentOfAssistantText(t *testing.T) {
	r := offlineTurn(t)
	startOfflineTurn(t, r)
	raw := []byte(`{"method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","item":{"type":"commandExecution","id":"command-1","status":"failed","exitCode":2,"command":"private command","aggregatedOutput":{"bad":"optional block"}}}}`)
	got, err := r.Apply(raw, projectionTime)
	if err != nil || len(got.Events) != 1 {
		t.Fatal(err)
	}
	var tool Tool
	if err = json.Unmarshal(got.Events[0].Data, &tool); err != nil {
		t.Fatal(err)
	}
	if tool.State != "failed" || tool.ExitCode == nil || *tool.ExitCode != 2 || tool.IsError == nil || !*tool.IsError || tool.InputPreview != "" || tool.OutputExcerpt != "" {
		t.Fatalf("bad failure: %+v", tool)
	}
	if again, err := r.Apply(raw, projectionTime); err != nil || again.Kind != "duplicate" || len(again.Events) != 0 {
		t.Fatal("duplicate counted")
	}
	got, err = r.Apply([]byte(`{"method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","item":{"type":"agentMessage","id":"message-1","text":"DONE"}}}`), projectionTime)
	if err != nil || got.Events[0].Type != "assistant.text" {
		t.Fatal(err)
	}
	got, err = r.Apply([]byte(`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","error":{"private":"not published"}}}}`), projectionTime)
	if err != nil || len(got.Events) != 1 {
		t.Fatal(err)
	}
	var completed Completed
	_ = json.Unmarshal(got.Events[0].Data, &completed)
	if completed.Outcome != "completed" || completed.ToolErrors != 1 || completed.Denials != 0 || got.Events[0].Seq != 3 {
		t.Fatalf("lost observed failure: %+v", completed)
	}
	if _, err = r.Apply(raw, projectionTime); !errors.Is(err, ErrCodexTurnClosed) {
		t.Fatal("late item accepted")
	}
}

func TestCodexOfflineIdleIsNotSuccessfulCompletion(t *testing.T) {
	r := offlineTurn(t)
	idle := []byte(`{"method":"thread/status/changed","params":{"threadId":"thread-1","status":{"type":"idle"}}}`)
	if _, err := r.Apply(idle, projectionTime); !errors.Is(err, ErrCodexTurnState) {
		t.Fatal("idle before start accepted")
	}
	startOfflineTurn(t, r)
	got, err := r.Apply(idle, projectionTime)
	if err != nil || len(got.Events) != 1 {
		t.Fatal(err)
	}
	var completed Completed
	_ = json.Unmarshal(got.Events[0].Data, &completed)
	if completed.Outcome != "unknown" || got.Events[0].Source != "driver" || got.Events[0].Fidelity != "derived" {
		t.Fatal("idle became success")
	}
}

func TestCodexOfflineRefusalsAreAtomicAndFinite(t *testing.T) {
	r := offlineTurn(t)
	startOfflineTurn(t, r)
	for _, raw := range []string{
		`{"method":"item/completed","params":{"threadId":"other-thread","turnId":"turn-1","item":{"type":"commandExecution","id":"command-1","status":"failed","exitCode":2}}}`,
		`{"method":"item/completed","params":{"threadId":"thread-1","turnId":"other-turn","item":{"type":"commandExecution","id":"command-1","status":"failed","exitCode":2}}}`,
		`{"method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","item":{"type":"commandExecution","id":"command-1","status":"completed","exitCode":"private diagnostic"}}}`,
		`{"method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","item":{"type":"commandExecution","id":"command-1","status":"failed","status":"completed"}}}`,
		`{"id":1,"method":"item/commandExecution/requestApproval","params":{"private":"request"}}`,
	} {
		got, err := r.Apply([]byte(raw), projectionTime)
		if err == nil || len(got.Events) != 0 || strings.Contains(err.Error(), "private") {
			t.Fatalf("bad refusal: %+v %v", got, err)
		}
	}
	got, err := r.Apply([]byte(`{"method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","item":{"type":"commandExecution","id":"command-1","status":"completed","exitCode":0}}}`), projectionTime)
	if err != nil || got.Events[0].Seq != 1 {
		t.Fatal("invalid messages mutated sequence")
	}
}

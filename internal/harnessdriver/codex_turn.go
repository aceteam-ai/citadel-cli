package harnessdriver

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
	"unicode/utf8"
)

var (
	ErrCodexNotification    = errors.New("harness_codex_notification_invalid")
	ErrCodexUnsupported     = errors.New("harness_codex_notification_unsupported")
	ErrCodexIdentity        = errors.New("harness_codex_notification_identity_mismatch")
	ErrCodexTurnState       = errors.New("harness_codex_turn_state_invalid")
	ErrCodexTurnClosed      = errors.New("harness_codex_turn_closed")
	ErrCodexReplay          = errors.New("harness_codex_item_replay_conflict")
	ErrCodexProjectionLimit = errors.New("harness_codex_projection_limit")
)

const maxCodexItems = 1024
const maxProjectionSeq = uint64(9007199254740991)

// CodexTurnIdentity is caller-supplied correlation, not native identity proof.
// No field here can clear a registry, permission or native isolation hold.
type CodexTurnIdentity struct {
	SessionName string
	Incarnation string
	ThreadID    string
	TurnID      string
	FirstSeq    uint64
}

// CodexProjection contains explicit content events, never safe diagnostics.
// Kind is a finite local disposition. The reducer has no transport or caller
// outside synthetic tests. It does not execute or resolve any request.
type CodexProjection struct {
	Kind   string
	Events []Event
}

// CodexTurn reduces an explicitly selected supplied turn stream. It owns no
// vendor process, configuration, connection or launch permission. Counts are
// observed unique terminal command items, not proof of an exhaustive stream.
type CodexTurn struct {
	mu                  sync.Mutex
	identity            CodexTurnIdentity
	next                uint64
	started, closed     bool
	seen                map[string][32]byte
	toolErrors, denials uint64
	lastObserved        time.Time
}

func (*CodexTurn) String() string               { return "offline turn projection (redacted)" }
func (*CodexTurn) GoString() string             { return "offline turn projection (redacted)" }
func (*CodexTurn) MarshalJSON() ([]byte, error) { return []byte("{}"), nil }

func NewCodexTurn(identity CodexTurnIdentity) (*CodexTurn, error) {
	if identity.ThreadID == "" || identity.TurnID == "" {
		return nil, ErrCodexIdentity
	}
	t := &CodexTurn{identity: identity, next: identity.FirstSeq, seen: make(map[string][32]byte)}
	// Reuse the contract for bounded envelope identifiers and sequence values.
	if _, err := t.makeEvent("session.error", Failure{Code: "unsupported"}, time.Unix(0, 0).UTC(), true); err != nil {
		return nil, ErrCodexIdentity
	}
	return t, nil
}

// Apply accepts only the documented item/turn subset. The caller must establish
// source provenance and ordered complete delivery separately. Malformed input,
// conflicting replays and unknown requests return finite errors with no state
// mutation. Optional native command/output/error detail is never published.
func (t *CodexTurn) Apply(raw []byte, observedAt time.Time) (CodexProjection, error) {
	message, err := codexObject(raw, true)
	if err != nil {
		return CodexProjection{}, err
	}
	if _, ok := message["id"]; ok {
		return CodexProjection{}, ErrCodexUnsupported
	}
	if _, ok := message["result"]; ok {
		return CodexProjection{}, ErrCodexUnsupported
	}
	if _, ok := message["error"]; ok {
		return CodexProjection{}, ErrCodexUnsupported
	}
	method, err := codexString(message, "method")
	if err != nil {
		return CodexProjection{}, err
	}
	params, err := codexObject(message["params"], false)
	if err != nil {
		return CodexProjection{}, err
	}
	thread, err := codexString(params, "threadId")
	if err != nil {
		return CodexProjection{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if observedAt.IsZero() || observedAt.Year() < 1970 || observedAt.Year() > 9999 || observedAt.Before(t.lastObserved) {
		return CodexProjection{}, ErrCodexTurnState
	}
	if thread != t.identity.ThreadID {
		return CodexProjection{}, ErrCodexIdentity
	}
	if t.closed {
		return CodexProjection{}, ErrCodexTurnClosed
	}
	if supplied, ok := params["turnId"]; ok {
		var id string
		if json.Unmarshal(supplied, &id) != nil || id != t.identity.TurnID {
			return CodexProjection{}, ErrCodexIdentity
		}
	}
	switch method {
	case "turn/started", "turn/completed":
		turn, err := codexObject(params["turn"], false)
		if err != nil {
			return CodexProjection{}, err
		}
		id, err := codexString(turn, "id")
		if err != nil {
			return CodexProjection{}, err
		}
		if id != t.identity.TurnID {
			return CodexProjection{}, ErrCodexIdentity
		}
		status, err := codexString(turn, "status")
		if err != nil {
			return CodexProjection{}, err
		}
		if method == "turn/started" {
			if status != "inProgress" {
				return CodexProjection{}, ErrCodexNotification
			}
			if t.started {
				return CodexProjection{Kind: "duplicate"}, nil
			}
			t.started = true
			t.lastObserved = observedAt
			return CodexProjection{Kind: "turn_started"}, nil
		}
		if !t.started {
			return CodexProjection{}, ErrCodexTurnState
		}
		switch status {
		case "completed", "failed", "interrupted":
		default:
			return CodexProjection{}, ErrCodexUnsupported
		}
		return t.finish(status, observedAt, false)
	case "thread/status/changed":
		if !t.started {
			return CodexProjection{}, ErrCodexTurnState
		}
		status, err := codexObject(params["status"], false)
		if err != nil {
			return CodexProjection{}, err
		}
		kind, err := codexString(status, "type")
		if err != nil {
			return CodexProjection{}, err
		}
		if kind != "idle" {
			return CodexProjection{}, ErrCodexUnsupported
		}
		// Idle does not carry a turn outcome, so it cannot become success.
		return t.finish("unknown", observedAt, true)
	case "item/completed", "item/agentMessage/delta":
		if !t.started {
			return CodexProjection{}, ErrCodexTurnState
		}
		if id, err := codexString(params, "turnId"); err != nil || id != t.identity.TurnID {
			return CodexProjection{}, ErrCodexIdentity
		}
		if method == "item/agentMessage/delta" {
			id, err := codexString(params, "itemId")
			if err != nil {
				return CodexProjection{}, err
			}
			text, err := codexText(params, "delta")
			if err != nil {
				return CodexProjection{}, err
			}
			e, err := t.makeEvent("assistant.delta", Text{Text: text, ItemID: id}, observedAt, false)
			if err != nil {
				return CodexProjection{}, err
			}
			t.next++
			t.lastObserved = observedAt
			return CodexProjection{Kind: "mapped", Events: []Event{e}}, nil
		}
		return t.item(params, observedAt)
	default:
		return CodexProjection{}, ErrCodexUnsupported
	}
}

func (t *CodexTurn) item(params map[string]json.RawMessage, observedAt time.Time) (CodexProjection, error) {
	item, err := codexObject(params["item"], false)
	if err != nil {
		return CodexProjection{}, err
	}
	id, err := codexString(item, "id")
	if err != nil {
		return CodexProjection{}, err
	}
	kind, err := codexString(item, "type")
	if err != nil {
		return CodexProjection{}, err
	}
	var event Event
	var failed, declined bool
	switch kind {
	case "commandExecution":
		status, err := codexString(item, "status")
		if err != nil {
			return CodexProjection{}, err
		}
		exit, err := codexExit(item["exitCode"])
		if err != nil {
			return CodexProjection{}, err
		}
		state := "completed"
		switch status {
		case "completed":
			failed = exit != nil && *exit != 0
		case "failed":
			failed = true
		case "declined":
			declined = true
		default:
			return CodexProjection{}, ErrCodexUnsupported
		}
		isError := failed || declined
		if isError {
			state = "failed"
		}
		event, err = t.makeEvent("tool.activity", Tool{ID: id, State: state, ExitCode: exit, IsError: &isError}, observedAt, false)
	case "agentMessage":
		text, textErr := codexText(item, "text")
		if textErr != nil {
			return CodexProjection{}, textErr
		}
		event, err = t.makeEvent("assistant.text", Text{Text: text, ItemID: id}, observedAt, false)
	default:
		return CodexProjection{}, ErrCodexUnsupported
	}
	if err != nil {
		return CodexProjection{}, err
	}
	// Store only a hash of the projected semantic content, never native bodies.
	digest := sha256.Sum256(append([]byte(event.Type+"\x00"), event.Data...))
	if before, ok := t.seen[id]; ok {
		if before != digest {
			return CodexProjection{}, ErrCodexReplay
		}
		return CodexProjection{Kind: "duplicate"}, nil
	}
	if len(t.seen) >= maxCodexItems {
		return CodexProjection{}, ErrCodexProjectionLimit
	}
	t.seen[id] = digest
	if failed {
		t.toolErrors++
	}
	if declined {
		t.denials++
	}
	t.next++
	t.lastObserved = observedAt
	return CodexProjection{Kind: "mapped", Events: []Event{event}}, nil
}

func (t *CodexTurn) finish(outcome string, at time.Time, derived bool) (CodexProjection, error) {
	e, err := t.makeEvent("turn.completed", Completed{Outcome: outcome, ToolErrors: t.toolErrors, Denials: t.denials}, at, derived)
	if err != nil {
		return CodexProjection{}, err
	}
	t.next++
	t.closed = true
	t.lastObserved = at
	return CodexProjection{Kind: "mapped", Events: []Event{e}}, nil
}

func (t *CodexTurn) makeEvent(kind string, payload any, at time.Time, derived bool) (Event, error) {
	if t.next < 1 || t.next > maxProjectionSeq {
		return Event{}, ErrCodexProjectionLimit
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return Event{}, ErrCodexNotification
	}
	e := Event{Protocol: Version, Type: kind, SessionName: t.identity.SessionName, Incarnation: t.identity.Incarnation, NativeID: t.identity.ThreadID, TurnID: t.identity.TurnID, Seq: t.next, TS: at, Source: "native", Fidelity: "structured", Data: data}
	if derived {
		e.Source = "driver"
		e.Fidelity = "derived"
	}
	if err = e.Validate(); err != nil {
		return Event{}, ErrCodexProjectionLimit
	}
	return e, nil
}

func codexObject(raw []byte, check bool) (map[string]json.RawMessage, error) {
	if len(raw) == 0 || len(raw) > MaxLineBytes || !utf8.Valid(raw) {
		return nil, ErrCodexNotification
	}
	if check {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		if uniqueValue(d, 0, new(int)) != nil {
			return nil, ErrCodexNotification
		}
		if _, err := d.Token(); !errors.Is(err, io.EOF) {
			return nil, ErrCodexNotification
		}
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return nil, ErrCodexNotification
	}
	return object, nil
}

func codexString(object map[string]json.RawMessage, key string) (string, error) {
	s, err := codexText(object, key)
	if err != nil || s == "" {
		return "", ErrCodexNotification
	}
	return s, nil
}
func codexText(object map[string]json.RawMessage, key string) (string, error) {
	raw, ok := object[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", ErrCodexNotification
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", ErrCodexNotification
	}
	return s, nil
}
func codexExit(raw json.RawMessage) (*int, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var n int64
	if json.Unmarshal(raw, &n) != nil || n < -2147483648 || n > 2147483647 {
		return nil, ErrCodexNotification
	}
	i := int(n)
	return &i, nil
}

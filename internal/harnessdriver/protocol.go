// Package harnessdriver defines an offline candidate harness-driver/1 contract.
// It supplies no vendor process, authenticated transport, admission or routing.
package harnessdriver

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

const Version = "harness-driver/1"
const MaxEnvelopeBytes = 64 << 10

var ErrProtocol = errors.New("harness_driver_invalid_protocol")

// The candidate wire contract is embedded so runtime validation
// and the fixtures use the same artifact. No remote schema resolution is used.
//
//go:embed protocol.schema.json
var schemaJSON string

// SchemaJSON returns the embedded candidate contract without exposing mutable
// validation configuration.
func SchemaJSON() string { return schemaJSON }

type Event struct {
	Protocol    string          `json:"protocol"`
	Type        string          `json:"type"`
	SessionName string          `json:"session_name"`
	Incarnation string          `json:"incarnation"`
	NativeID    string          `json:"native_id"`
	TurnID      string          `json:"turn_id"`
	Seq         uint64          `json:"seq"`
	TS          time.Time       `json:"ts"`
	Source      string          `json:"source"`
	Fidelity    string          `json:"fidelity"`
	Data        json.RawMessage `json:"data"`
}

type Command struct {
	Protocol    string          `json:"protocol"`
	Type        string          `json:"type"`
	SessionName string          `json:"session_name"`
	Incarnation string          `json:"incarnation"`
	NativeID    string          `json:"native_id"`
	TurnID      string          `json:"turn_id"`
	RequestID   string          `json:"request_id"`
	Data        json.RawMessage `json:"data"`
}

type Start struct {
	Workspace string `json:"workspace"`
	Model     string `json:"model"`
}
type Submit struct {
	Text string `json:"text"`
}
type Respond struct {
	ApprovalID string `json:"approval_id"`
	Decision   string `json:"decision"`
}
type Empty struct{}
type Text struct {
	Text   string `json:"text"`
	ItemID string `json:"item_id,omitempty"`
}
type Tool struct {
	ID            string `json:"id"`
	State         string `json:"state"`
	ExitCode      *int   `json:"exit_code"`
	IsError       *bool  `json:"is_error"`
	InputPreview  string `json:"input_preview"`
	OutputExcerpt string `json:"output_excerpt"`
}
type Approval struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Choices []string `json:"choices"`
}
type Resolution struct {
	ID       string `json:"id"`
	Decision string `json:"decision"`
}
type Usage struct {
	Model        string  `json:"model"`
	SeatRef      string  `json:"seat_ref,omitempty"`
	ModelActual  string  `json:"model_actual,omitempty"`
	InputTokens  *uint64 `json:"input_tokens"`
	OutputTokens *uint64 `json:"output_tokens"`
	CachedTokens *uint64 `json:"cached_tokens"`
}
type Quota struct {
	Window      string     `json:"window"`
	UsedPercent *float64   `json:"used_percent"`
	ResetsAt    *time.Time `json:"resets_at"`
}
type Models struct {
	IDs []string `json:"ids"`
}
type Completed struct {
	Outcome     string `json:"outcome"`
	ToolErrors  uint64 `json:"tool_errors"`
	Denials     uint64 `json:"denials"`
	CountsScope string `json:"counts_scope,omitempty"`
}
type Failure struct {
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
}

var schemas struct {
	sync.Once
	event, command *jsonschema.Schema
	quota, usage   *jsonschema.Schema
	err            error
}

func contract(event bool) (*jsonschema.Schema, error) {
	schemas.Do(func() {
		compiler := jsonschema.NewCompiler()
		compiler.AssertFormat = true
		compiler.LoadURL = func(string) (io.ReadCloser, error) { return nil, ErrProtocol }
		const base = "https://schemas.example.invalid/harness-driver/1"
		if schemas.err = compiler.AddResource(base, bytes.NewBufferString(schemaJSON)); schemas.err != nil {
			return
		}
		if schemas.event, schemas.err = compiler.Compile(base + "#/$defs/event"); schemas.err != nil {
			return
		}
		if schemas.command, schemas.err = compiler.Compile(base + "#/$defs/command"); schemas.err != nil {
			return
		}
		if schemas.quota, schemas.err = compiler.Compile(base + "#/$defs/quota"); schemas.err != nil {
			return
		}
		schemas.usage, schemas.err = compiler.Compile(base + "#/$defs/usage")
	})
	if schemas.err != nil {
		return nil, ErrProtocol
	}
	if event {
		return schemas.event, nil
	}
	return schemas.command, nil
}

func decode(raw []byte, event bool, target any) error {
	if len(raw) == 0 || len(raw) > MaxEnvelopeBytes || !utf8.Valid(raw) {
		return ErrProtocol
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := uniqueValue(d, 0, new(int)); err != nil {
		return ErrProtocol
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return ErrProtocol
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if err := d.Decode(&value); err != nil {
		return ErrProtocol
	}
	s, err := contract(event)
	if err != nil || s.Validate(value) != nil {
		return ErrProtocol
	}
	if err = json.Unmarshal(raw, target); err != nil {
		return ErrProtocol
	}
	return nil
}

// uniqueValue rejects duplicate keys before decoding into maps can erase them.
// Depth and token bounds also apply to unknown/optional payloads.
func uniqueValue(d *json.Decoder, depth int, budget *int) error {
	(*budget)++
	if depth > 16 || *budget > 16384 {
		return ErrProtocol
	}
	t, err := d.Token()
	if err != nil {
		return ErrProtocol
	}
	switch t {
	case json.Delim('{'):
		keys := map[string]bool{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return ErrProtocol
			}
			name, ok := k.(string)
			if !ok || keys[name] {
				return ErrProtocol
			}
			keys[name] = true
			if err = uniqueValue(d, depth+1, budget); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return ErrProtocol
		}
	case json.Delim('['):
		for d.More() {
			if err := uniqueValue(d, depth+1, budget); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return ErrProtocol
		}
	case json.Delim('}'), json.Delim(']'):
		return ErrProtocol
	}
	return nil
}

func DecodeEvent(raw []byte) (Event, error) {
	var e Event
	if err := decode(raw, true, &e); err != nil {
		return Event{}, err
	}
	return e, nil
}
func DecodeCommand(raw []byte) (Command, error) {
	var c Command
	if err := decode(raw, false, &c); err != nil {
		return Command{}, err
	}
	return c, nil
}
func (e Event) Validate() error {
	if !boundedParts(len(e.Data), e.Protocol, e.Type, e.SessionName, e.Incarnation, e.NativeID, e.TurnID, e.Source, e.Fidelity) {
		return ErrProtocol
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return ErrProtocol
	}
	_, err = DecodeEvent(raw)
	return err
}
func (c Command) Validate() error {
	if !boundedParts(len(c.Data), c.Protocol, c.Type, c.SessionName, c.Incarnation, c.NativeID, c.TurnID, c.RequestID) {
		return ErrProtocol
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return ErrProtocol
	}
	_, err = DecodeCommand(raw)
	return err
}

func boundedParts(size int, fields ...string) bool {
	if size > MaxEnvelopeBytes {
		return false
	}
	remaining := MaxEnvelopeBytes - size
	for _, field := range fields {
		if len(field) > remaining {
			return false
		}
		remaining -= len(field)
	}
	return true
}

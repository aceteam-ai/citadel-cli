package jobs

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/memorygit"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// MemoryGitHandler is source-only: no production registry or advertised
// capability constructs it. A future adapter must authenticate/authorize the
// scope and attach the existing exact-node Agent-window passcode server-side.
// Repository/scopes and strict passcode dependencies come from trusted node
// configuration through NewMemoryGitHandler. The zero value refuses execution.
type MemoryGitHandler struct {
	engine   *memorygit.Engine
	enabled  func() bool
	state    func() (PasscodeState, error)
	throttle *PasscodeThrottle
}

// NewMemoryGitHandler requires live permission, strict State and the same
// configured throttle object used by the node's shell/desktop/workflow gates.
// It neither invokes the callbacks nor accepts the legacy passcode callbacks.
// A nonnil pointer establishes wiring presence, not identity or effective
// throttle tuning; supplying the actual configured shared object is the caller's
// trusted wiring obligation. This does not register or advertise the handler.
func NewMemoryGitHandler(engine *memorygit.Engine, liveEnabled func() bool, strictState func() (PasscodeState, error), sharedThrottle *PasscodeThrottle) (*MemoryGitHandler, error) {
	if engine == nil || liveEnabled == nil || strictState == nil || sharedThrottle == nil {
		return nil, &memorygit.Error{Code: "invalid_configuration"}
	}
	return &MemoryGitHandler{engine: engine, enabled: liveEnabled, state: strictState, throttle: sharedThrottle}, nil
}

func (h *MemoryGitHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	if job == nil || job.Type != memorygit.JobType {
		return nil, &memorygit.Error{Code: "invalid_request"}
	}
	// Do not allow nil/zero-value or incomplete in-package construction to
	// bypass the same prerequisites required by the constructor.
	if h == nil || h.engine == nil || h.enabled == nil || h.state == nil || h.throttle == nil {
		return nil, &memorygit.Error{Code: "invalid_configuration"}
	}
	gate := ShellGate{Enabled: h.enabled, State: h.state, Throttle: h.throttle}
	if reason := ShellGateReason(gate, job.Payload[ShellPasscodePayloadKey]); reason != "" {
		return nil, &ShellRefusal{Reason: reason, Message: shellCommandRefusalMessage(reason)}
	}
	for key := range job.Payload {
		if key != "request" && key != ShellPasscodePayloadKey {
			return nil, &memorygit.Error{Code: "invalid_request"}
		}
	}
	raw := job.Payload["request"]
	if len(raw) == 0 || len(raw) > memorygit.MaxRequestBytes {
		return nil, &memorygit.Error{Code: "invalid_request"}
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	// encoding/json otherwise accepts duplicate and case-insensitive keys.
	// Decode the exact wire schema so two adapters cannot interpret one
	// request differently. Values are decoded to the typed Request once.
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, &memorygit.Error{Code: "invalid_request"}
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, &memorygit.Error{Code: "invalid_request"}
		}
		switch key {
		case "version", "node_id", "scope", "operation", "name", "revision", "from", "to", "limit":
		default:
			return nil, &memorygit.Error{Code: "invalid_request"}
		}
		if _, exists := fields[key]; exists {
			return nil, &memorygit.Error{Code: "invalid_request"}
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil || string(value) == "null" {
			return nil, &memorygit.Error{Code: "invalid_request"}
		}
		fields[key] = value
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return nil, &memorygit.Error{Code: "invalid_request"}
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, &memorygit.Error{Code: "invalid_request"}
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, &memorygit.Error{Code: "invalid_request"}
	}
	var request memorygit.Request
	if err := json.Unmarshal(encoded, &request); err != nil {
		return nil, &memorygit.Error{Code: "invalid_request"}
	}
	result, err := h.engine.Execute(ctx.Context(), request)
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

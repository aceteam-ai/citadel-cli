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
// Engine repository/scopes and Gate are supplied from trusted node config.
type MemoryGitHandler struct {
	Engine *memorygit.Engine
	Gate   ShellGate
}

func (h *MemoryGitHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	if job == nil || job.Type != memorygit.JobType {
		return nil, &memorygit.Error{Code: "invalid_request"}
	}
	// Require an explicit live permission callback. Unlike legacy shell's
	// Disabled=false default, forgetting to wire this gate cannot enable it.
	if h.Gate.Enabled == nil {
		return nil, &ShellRefusal{Reason: ReasonShellDisabled, Message: msgShellDisabled}
	}
	if reason := ShellGateReason(h.Gate, job.Payload[ShellPasscodePayloadKey]); reason != "" {
		return nil, &ShellRefusal{Reason: reason, Message: shellCommandRefusalMessage(reason)}
	}
	for key := range job.Payload {
		if key != "request" && key != ShellPasscodePayloadKey {
			return nil, &memorygit.Error{Code: "invalid_request"}
		}
	}
	raw := job.Payload["request"]
	if len(raw) == 0 || len(raw) > memorygit.MaxRequestBytes || h.Engine == nil {
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
	result, err := h.Engine.Execute(ctx.Context(), request)
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

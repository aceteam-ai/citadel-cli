package localchat

import (
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// EngineChoice is a single selectable chat target: a running engine and one of
// its served models, plus the localhost port to reach it.
type EngineChoice struct {
	Engine string // vllm, ollama, llamacpp, bonsai
	Model  string // served model id (may be empty for a running engine with no reported model)
	Port   int
}

// Label is the human-readable line shown in the picker.
func (c EngineChoice) Label() string {
	if c.Model == "" {
		return fmt.Sprintf("%s (default model) — localhost:%d", c.Engine, c.Port)
	}
	return fmt.Sprintf("%s — %s — localhost:%d", c.Engine, c.Model, c.Port)
}

// BuildChoices flattens discovered local engines into one choice per served
// model. An engine that is running but reports no loaded model still yields a
// single choice with an empty Model: llama.cpp/bonsai ignore the request's
// model field and serve their loaded default, so chatting is still possible.
// The order follows the engine discovery order, models in reported order.
func BuildChoices(engines []status.LocalEngine) []EngineChoice {
	var out []EngineChoice
	for _, e := range engines {
		if len(e.Models) == 0 {
			out = append(out, EngineChoice{Engine: e.Name, Model: "", Port: e.Port})
			continue
		}
		for _, m := range e.Models {
			out = append(out, EngineChoice{Engine: e.Name, Model: m, Port: e.Port})
		}
	}
	return out
}

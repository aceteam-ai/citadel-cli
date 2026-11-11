package redisapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestAPIJobRenderingOmitsTheNodePasscode(t *testing.T) {
	const pin = "synthetic-node-passcode-4821"
	job := Job{Type: "SHELL_COMMAND", Payload: map[string]any{"passcode": pin, "command": "true"}, RawPayload: []byte(pin), RawData: map[string]any{"payload": pin}}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	for _, rendered := range []string{string(encoded), fmt.Sprintf("%v", job), fmt.Sprintf("%+v", job)} {
		if strings.Contains(rendered, pin) {
			t.Fatalf("job rendering retained the passcode: %s", rendered)
		}
	}
}

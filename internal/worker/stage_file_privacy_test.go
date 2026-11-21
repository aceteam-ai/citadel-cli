package worker

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A stage_file payload carries the user's file as base64: never in a worker
// Job rendering (citadel-cli#1721).
func TestWorkerJobRenderingOmitsStagedFileBytes(t *testing.T) {
	const body = "U1lOVEhFVElDLUZJTEUtQllURVMtMTIzNDU2"
	job := Job{ID: "j", Type: JobTypeCobrowseSession, Payload: map[string]any{"bytes_b64": body, "action": "stage_file", "name": "filing.pdf"}, RawPayload: []byte(body)}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	for _, rendered := range []string{string(encoded), fmt.Sprintf("%v", job), fmt.Sprintf("%+v", job)} {
		if strings.Contains(rendered, body) {
			t.Fatalf("job rendering retained the file bytes: %s", rendered)
		}
		if !strings.Contains(rendered, "filing.pdf") {
			t.Fatalf("job rendering lost the other fields: %s", rendered)
		}
	}
}

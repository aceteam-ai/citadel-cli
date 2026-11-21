package redisapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestAPIJobRenderingOmitsStagedFileBytes(t *testing.T) {
	const body = "U1lOVEhFVElDLUZJTEUtQllURVMtMTIzNDU2"
	job := Job{Type: "COBROWSE_SESSION", Payload: map[string]any{"bytes_b64": body, "name": "filing.pdf"}, RawPayload: []byte(body), RawData: map[string]any{"payload": body}}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	for _, rendered := range []string{string(encoded), fmt.Sprintf("%v", job), fmt.Sprintf("%+v", job)} {
		if strings.Contains(rendered, body) {
			t.Fatalf("job rendering retained the file bytes: %s", rendered)
		}
	}
}

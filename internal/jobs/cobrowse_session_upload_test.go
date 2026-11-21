package jobs

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// runSessionFilesOn runs a job through a handler whose Files permission is on.
func runSessionFilesOn(t *testing.T, payload map[string]string) ([]byte, error) {
	t.Helper()
	h := NewCobrowseSessionHandler()
	h.Enabled = func() bool { return true }
	return h.Execute(JobContext{}, &nexus.Job{ID: "test", Type: "COBROWSE_SESSION", Payload: payload})
}

func TestCobrowseSession_StageFileRequiresFields(t *testing.T) {
	cases := []map[string]string{
		{"action": CobrowseSessionActionStageFile},
		{"action": CobrowseSessionActionStageFile, "session_id": "cb-x"},
		{"action": CobrowseSessionActionStageFile, "name": "a.pdf"},
	}
	for _, p := range cases {
		if _, err := runSessionFilesOn(t, p); err == nil {
			t.Errorf("%v should be refused", p)
		}
	}
}

func TestCobrowseSession_StageFileRefusals(t *testing.T) {
	good := base64.StdEncoding.EncodeToString([]byte("hello"))
	// Bad base64 is refused without echoing the payload.
	_, err := runSessionFilesOn(t, map[string]string{"action": CobrowseSessionActionStageFile, "session_id": "cb-x", "name": "a.pdf", CobrowseStageBytesKey: "!!not base64!!"})
	if err == nil || strings.Contains(err.Error(), "not base64!!") {
		t.Fatalf("bad base64: %v", err)
	}
	// Oversize is refused before decoding.
	big := strings.Repeat("A", base64.StdEncoding.EncodedLen(platform.MaxCobrowseStagedFileBytes)+8)
	_, err = runSessionFilesOn(t, map[string]string{"action": CobrowseSessionActionStageFile, "session_id": "cb-x", "name": "a.pdf", CobrowseStageBytesKey: big})
	if !errors.Is(err, platform.ErrStagedTooLarge) {
		t.Fatalf("oversize: %v", err)
	}
	// A bad name is refused before the session is even looked up.
	_, err = runSessionFilesOn(t, map[string]string{"action": CobrowseSessionActionStageFile, "session_id": "cb-x", "name": "../a.pdf", CobrowseStageBytesKey: good})
	if !errors.Is(err, platform.ErrStagedNameInvalid) {
		t.Fatalf("bad name: %v", err)
	}
}

func TestCobrowseSession_SetFileInputPayloadValidation(t *testing.T) {
	cases := []map[string]string{
		{"action": CobrowseSessionActionSetFileInput},
		{"action": CobrowseSessionActionSetFileInput, "session_id": "cb-x"},
		{"action": CobrowseSessionActionSetFileInput, "session_id": "cb-x", "staged_name": "a.pdf"},
		{"action": CobrowseSessionActionSetFileInput, "session_id": "cb-x", "staged_name": "a.pdf", "selector": "#f", "backend_node_id": "4"},
		{"action": CobrowseSessionActionSetFileInput, "session_id": "cb-x", "staged_name": "a.pdf", "backend_node_id": "zero"},
		{"action": CobrowseSessionActionSetFileInput, "session_id": "cb-x", "staged_name": "a.pdf", "backend_node_id": "-3"},
	}
	for _, p := range cases {
		if _, err := runSessionFilesOn(t, p); err == nil {
			t.Errorf("%v should be refused", p)
		}
	}
}

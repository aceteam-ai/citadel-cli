package jobs

import (
	"strings"
	"testing"
)

func TestCobrowseSession_SetFileInputRequiresExpectedURL(t *testing.T) {
	_, err := runSessionFilesOn(t, map[string]string{"action": CobrowseSessionActionSetFileInput, "session_id": "cb-x", "staged_name": "a.pdf", "selector": "#f"})
	if err == nil || !strings.Contains(err.Error(), "expected_url") {
		t.Fatalf("got %v", err)
	}
}

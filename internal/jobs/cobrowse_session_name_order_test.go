package jobs

import (
	"errors"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// The name is judged before the payload is decoded: a bad name with garbage
// bytes reports the name, so no decode work is spent on it.
func TestCobrowseSession_StageFileValidatesNameBeforeDecoding(t *testing.T) {
	_, err := runSessionFilesOn(t, map[string]string{
		"action": CobrowseSessionActionStageFile, "session_id": "cb-x", "name": "../a.pdf", CobrowseStageBytesKey: "!!not base64!!",
	})
	if !errors.Is(err, platform.ErrStagedNameInvalid) {
		t.Fatalf("got %v, want the name refusal", err)
	}
}

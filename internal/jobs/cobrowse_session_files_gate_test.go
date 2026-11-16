package jobs

import (
	"encoding/base64"
	"errors"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

func runGated(h *CobrowseSessionHandler, payload map[string]string) error {
	_, err := h.Execute(JobContext{}, &nexus.Job{ID: "t", Type: "COBROWSE_SESSION", Payload: payload})
	return err
}

func filePayloads() []map[string]string {
	b := base64.StdEncoding.EncodeToString([]byte("x"))
	return []map[string]string{
		{"action": CobrowseSessionActionStageFile, "session_id": "cb-none", "name": "a.pdf", CobrowseStageBytesKey: b},
		{"action": CobrowseSessionActionSetFileInput, "session_id": "cb-none", "staged_name": "a.pdf", "selector": "#f"},
	}
}

// Files off refuses both actions with the terminal files_disabled error before
// the session is even looked up, so nothing can reach a staging dir.
func TestCobrowseSession_FileActionsRefusedWhenFilesOff(t *testing.T) {
	for _, h := range []*CobrowseSessionHandler{
		{Disabled: true},
		{Enabled: func() bool { return false }},
		{Disabled: false, Enabled: func() bool { return false }},
	} {
		for _, p := range filePayloads() {
			if err := runGated(h, p); !errors.Is(err, ErrFilesDisabled) {
				t.Errorf("%s: want ErrFilesDisabled, got %v", p["action"], err)
			}
		}
	}
}

// Files on passes the gate (the error is then about the unknown session), and
// the permission is read live on every job.
func TestCobrowseSession_FileActionsAllowedWhenFilesOnAndLive(t *testing.T) {
	on := true
	h := &CobrowseSessionHandler{Enabled: func() bool { return on }}
	for _, p := range filePayloads() {
		err := runGated(h, p)
		if err == nil || errors.Is(err, ErrFilesDisabled) {
			t.Errorf("%s with Files on: got %v", p["action"], err)
		}
	}
	on = false
	for _, p := range filePayloads() {
		if err := runGated(h, p); !errors.Is(err, ErrFilesDisabled) {
			t.Errorf("%s after Files turned off: got %v", p["action"], err)
		}
	}
	// Other session actions are not gated by Files.
	if err := runGated(&CobrowseSessionHandler{Disabled: true}, map[string]string{"action": CobrowseSessionActionStatus, "session_id": "cb-none"}); errors.Is(err, ErrFilesDisabled) {
		t.Error("status must not need the Files permission")
	}
}

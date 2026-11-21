package jobs

import (
	"strings"
	"testing"
)

// No COBROWSE_SESSION action reaches the viewer's human file attach: the
// handler has no such action, so a job (the only path a hosted MCP tool has to
// the node) cannot attach as the human. set_file_input stays the agent's action
// and is refused while the human drives.
func TestCobrowseSession_NoActionReachesHumanFileAttach(t *testing.T) {
	for _, action := range []string{"file_attach", "human_attach", "attach_file", "attach_staged_file", "human_file_attach", "answer_file_chooser"} {
		_, err := runSession(t, map[string]string{
			"action": action, "session_id": "cb-1", "staged_name": "a.pdf", "backend_node_id": "7",
		})
		if err == nil || !strings.Contains(err.Error(), "unknown browser-session action") {
			t.Errorf("action %q: err = %v, want an unknown-action refusal", action, err)
		}
	}
}

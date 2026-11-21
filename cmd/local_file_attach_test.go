package cmd

import (
	"regexp"
	"testing"
)

// No local MCP tool can attach a staged file as the human (citadel-cli#1723):
// that is a viewer-stream input only. Modelled on TestNoLocalMCPToolCanApprove.
func TestNoLocalMCPToolCanAttachAsHuman(t *testing.T) {
	rt := newLocalCobrowseRuntime(localCobrowseOptions{ownerDir: func() string { return t.TempDir() }, browser: newCmdFakeBrowser(), policy: allowAllURLs})
	defer rt.close()
	bad := regexp.MustCompile(`(?i)attach|chooser|file|upload|stage`)
	for _, tool := range newLocalCobrowseTools(rt) {
		if bad.MatchString(tool.Name) {
			t.Errorf("tool %s looks like a file attach path", tool.Name)
		}
	}
	for _, name := range []string{"local_cobrowse_file_attach", "local_cobrowse_attach_file", "local_cobrowse_set_file_input"} {
		rsp := runLocalBridge(t, rt, nil,
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":{"session_id":"cb-1","staged_name":"a.pdf","backend_node_id":7}}}`)
		if e, _ := rsp["1"]["error"].(map[string]any); e == nil || e["code"] != float64(-32601) {
			t.Fatalf("%s must be refused as unavailable: %v", name, rsp["1"])
		}
	}
}

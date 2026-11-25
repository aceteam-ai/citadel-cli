package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/localviewer"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// TestEnrolledBridgeCannotGrantOnAPosturePC is the end-to-end proof for a PC
// connected from its setup page: an agent on the enrolled `citadel mcp`
// bridge calls the backend's set-permission tools, which fall through to the
// backend, and the backend (simulated here) dispatches APPLY_DEVICE_CONFIG to
// the PC. The PC refuses every enable and the passcode push, and reports why.
func TestEnrolledBridgeCannotGrantOnAPosturePC(t *testing.T) {
	nodeDir := t.TempDir()
	if err := writeSetupPosture(nodeDir, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	handler := jobs.NewConfigHandler(nodeDir)
	handler.PermissionsDir = nodeDir

	// tool -> the APPLY_DEVICE_CONFIG field the backend sets for it.
	tools := map[string]map[string]any{
		"citadel_set_files_permission":   {"filesEnabled": true},
		"citadel_set_shell_permission":   {"shellEnabled": true, "nodePasscode": "2468"},
		"citadel_set_console_permission": {"consoleEnabled": true},
		"citadel_set_desktop_permission": {"desktopEnabled": true},
	}
	var results []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		cfg := map[string]any{"deviceName": "pc"}
		for k, v := range tools[req.Params.Name] {
			cfg[k] = v
		}
		payload, _ := json.Marshal(cfg)
		out, err := handler.Execute(jobs.JobContext{}, &nexus.Job{Type: "APPLY_DEVICE_CONFIG", Payload: map[string]string{"config": string(payload)}})
		if err != nil {
			t.Errorf("dispatch: %v", err)
		}
		results = append(results, string(out))
		text, _ := json.Marshal(string(out))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":{"content":[{"type":"text","text":` + string(text) + `}]}}`))
	}))
	defer backend.Close()

	bridge := &mcpBridge{apiKey: "test-key", endpointURL: backend.URL, mcpServer: "aceteam", httpClient: backend.Client()}
	i := 0
	for name := range tools {
		i++
		req := &jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(jsonString2(i)), Method: "tools/call",
			Params: json.RawMessage(`{"name":"` + name + `","arguments":{"node_id":"pc","enabled":true}}`)}
		if bridge.tryLocalToolsCall(req) {
			t.Fatalf("%s was served locally; it must reach the backend", name)
		}
		if _, err := bridge.forwardToBackend(req); err != nil {
			t.Fatalf("%s: forward: %v", name, err)
		}
	}
	if len(results) != len(tools) {
		t.Fatalf("the backend dispatched %d of %d calls", len(results), len(tools))
	}
	for _, out := range results {
		if !strings.Contains(out, "Refused:") {
			t.Errorf("no refusal reported: %q", out)
		}
	}
	p, err := config.LoadPermissionsStrict(nodeDir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Files || p.Shell || p.Console || p.Desktop || p.HasPasscode() || p.SetupPosture == "" || !p.CobrowsePublicOnly {
		t.Fatalf("a remote call changed the posture: %+v", p)
	}
}

// TestSetupPagePostureRetry: when saving the posture failed after a connect,
// Retry writes the full posture and then offers the worker.
func TestSetupPagePostureRetry(t *testing.T) {
	f := newSetupFixture(t)
	if code, _ := f.enrollCall("POST", "/api/setup/posture/retry", nil); code != http.StatusConflict {
		t.Fatalf("retry with nothing pending: %d", code)
	}
	if err := os.MkdirAll(f.nodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	permFile := filepath.Join(f.nodeDir, "permissions.yaml")
	if err := os.WriteFile(permFile, []byte("shell: [true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.setEnroll(connectedEnroll)
	var seen []string
	f.enrollCall("POST", "/api/setup/enroll/start", &seen)
	st := f.waitEnroll(&seen, stageIs(setupEnrollConnected))
	if !st.PostureRetry || st.WorkerOffer || st.Posture != setupPostureFailedCopy {
		t.Fatalf("failed posture: %+v", st)
	}
	// Still damaged: the retry reports the failure and keeps Retry offered.
	code, body := f.enrollCall("POST", "/api/setup/posture/retry", nil)
	var res localviewer.SetupActionResult
	if err := json.Unmarshal([]byte(body), &res); err != nil || code != http.StatusOK || res.OK {
		t.Fatalf("retry over a damaged file: %d %s", code, body)
	}
	if st := f.enrollState(&seen); !st.PostureRetry || st.WorkerOffer {
		t.Fatalf("after a failed retry: %+v", st)
	}
	// Fixed: the retry writes the full posture.
	if err := os.Remove(permFile); err != nil {
		t.Fatal(err)
	}
	code, body = f.enrollCall("POST", "/api/setup/posture/retry", nil)
	if err := json.Unmarshal([]byte(body), &res); err != nil || code != http.StatusOK || !res.OK {
		t.Fatalf("retry: %d %s", code, body)
	}
	p, err := config.LoadPermissionsStrict(f.nodeDir)
	if err != nil {
		t.Fatal(err)
	}
	if *p != (config.Permissions{CobrowsePublicOnly: true, SetupPosture: config.SetupPostureCobrowseOnly}) || config.LoadMeeting(f.meetingDir).MeetingEnabled {
		t.Fatalf("the retry must write the full posture: %+v", p)
	}
	if st := f.enrollState(&seen); st.PostureRetry || !st.WorkerOffer || st.Posture != setupPostureCopy {
		t.Fatalf("after a successful retry: %+v", st)
	}
}

package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/localaudit"
)

const auditSentinel = "S3ntinel-passphrase-do-not-log"

func TestLocalAuditRecordsEveryCallWithoutSecrets(t *testing.T) {
	ownerDir := t.TempDir()
	rt := newLocalCobrowseRuntime(localCobrowseOptions{ownerDir: func() string { return ownerDir }, browser: newCmdFakeBrowser(), policy: allowAllURLs})
	defer rt.close()
	one := func(line string) map[string]any {
		t.Helper()
		resp := runLocalBridge(t, rt, nil, line)
		for _, v := range resp {
			return v
		}
		t.Fatalf("no response to %s", line)
		return nil
	}
	one(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"Audit Harness"}}}`)
	sid := map[string]any{"session_id": "cb-1"}
	calls := []struct {
		tool string
		args map[string]any
	}{
		{"local_cobrowse_start", map[string]any{"url": "https://fixture.test/login?token=abc123", "goal": "check orders"}},
		{"local_cobrowse_type", map[string]any{"session_id": "cb-1", "text": auditSentinel}}, // refused: no grant
		{"local_cobrowse_status", sid},
	}
	for i, c := range calls {
		one(call(10+i, c.tool, c.args))
	}
	if err := rt.svc.Approve("cb-1", 0); err != nil { // the user's viewer action
		t.Fatal(err)
	}
	more := []struct {
		tool string
		args map[string]any
	}{
		{"local_cobrowse_type", map[string]any{"session_id": "cb-1", "text": auditSentinel}},
		{"local_cobrowse_navigate", map[string]any{"session_id": "cb-1", "url": "https://fixture.test/orders"}},
		{"local_cobrowse_click", map[string]any{"session_id": "cb-1", "selector": "#o1"}},
		{"local_cobrowse_key", map[string]any{"session_id": "cb-1", "key": "Enter"}},
		{"local_cobrowse_extract", map[string]any{"session_id": "cb-1", "selector": "#orders"}},
		{"local_cobrowse_screenshot", sid},
		{"local_cobrowse_handoff", map[string]any{"session_id": "cb-1", "reason": "log in"}},
		{"local_cobrowse_screenshot", sid}, // refused: the user drives
		{"local_cobrowse_resume", sid},
		{"local_cobrowse_wait", map[string]any{"session_id": "cb-1", "until": "approved", "timeout_seconds": 1}},
		{"local_cobrowse_stop", sid},
	}
	for i, c := range more {
		one(call(20+i, c.tool, c.args))
	}
	agentCalls := len(calls) + len(more)

	dir := filepath.Join(ownerDir, localaudit.DirName)
	recs, err := localaudit.Read(dir, localaudit.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	agent, user, refused := 0, 0, 0
	for _, r := range recs {
		switch r.Actor {
		case "agent":
			agent++
			if r.Agent != "Audit Harness" {
				t.Errorf("record %d lacks the MCP client name: %+v", r.Seq, r)
			}
		case "user":
			user++
		}
		if r.ResultingAction == "refused" {
			refused++
		}
		if r.Source != "local" {
			t.Errorf("source: %+v", r)
		}
	}
	if agent != agentCalls || user != 1 {
		t.Fatalf("want one record per call (%d agent, 1 user), got %d agent, %d user", agentCalls, agent, user)
	}
	if refused != 2 {
		t.Errorf("want the 2 refusals recorded, got %d", refused)
	}

	// Typed text and URL secrets never reach the log.
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, secret := range []string{auditSentinel, "abc123"} {
			if bytes.Contains(b, []byte(secret)) {
				t.Fatalf("%q leaked into %s", secret, f)
			}
		}
	}
	var out bytes.Buffer
	if err := runLocalAudit(dir, localAuditOptions{verify: true}, &out); err != nil || !strings.Contains(out.String(), "Audit log OK") {
		t.Fatalf("verify: %v %s", err, out.String())
	}
	out.Reset()
	if err := runLocalAudit(dir, localAuditOptions{session: "cb-1", limit: 5}, &out); err != nil || strings.Count(out.String(), "\n") != 5 {
		t.Fatalf("print: %v\n%s", err, out.String())
	}

	// An edited record fails verification.
	b, _ := os.ReadFile(files[0])
	if err := os.WriteFile(files[0], bytes.Replace(b, []byte(`"resulting_action":"refused"`), []byte(`"resulting_action":"ok"`), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runLocalAudit(dir, localAuditOptions{verify: true}, &out); err == nil || !strings.Contains(err.Error(), "modified") {
		t.Fatalf("edit must be detected, got %v", err)
	}
}

func TestLocalAuditWriteFailureTurnsCobrowseOff(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	ownerDir := t.TempDir()
	rt := newLocalCobrowseRuntime(localCobrowseOptions{ownerDir: func() string { return ownerDir }, browser: newCmdFakeBrowser(), policy: allowAllURLs})
	defer rt.close()
	if r := runLocalBridge(t, rt, nil, call(1, "local_cobrowse_start", nil)); r["1"] == nil {
		t.Fatal("start")
	}
	dir := filepath.Join(ownerDir, localaudit.DirName)
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	for _, f := range files {
		_ = os.Chmod(f, 0o400)
	}
	_ = os.Chmod(dir, 0o500)
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	runLocalBridge(t, rt, nil, call(2, "local_cobrowse_status", nil)) // this write fails
	resp := runLocalBridge(t, rt, nil, call(3, "local_cobrowse_status", nil))
	content, isErr := toolResult(t, resp["3"])
	if !isErr || !strings.Contains(firstText(content), "audit log") {
		t.Fatalf("co-browse must refuse once the audit log cannot be written: %v", resp["3"])
	}
}

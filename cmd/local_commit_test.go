package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/localaudit"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// No MCP tool can grant, approve, deny or revoke anything: consent has only
// human-side methods (the viewer), so an injected agent cannot reach it.
func TestNoLocalMCPToolCanApprove(t *testing.T) {
	rt := newLocalCobrowseRuntime(localCobrowseOptions{ownerDir: func() string { return t.TempDir() }, browser: newCmdFakeBrowser(), policy: allowAllURLs})
	defer rt.close()
	bad := regexp.MustCompile(`(?i)approve|grant|deny|revoke|consent|allow`)
	for _, tool := range newLocalCobrowseTools(rt) {
		if bad.MatchString(tool.Name) {
			t.Errorf("tool %s looks like a consent path", tool.Name)
		}
	}
	rsp := runLocalBridge(t, rt, nil,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"local_cobrowse_approve_commit","arguments":{"session_id":"cb-1","commit":"x"}}}`)
	// An unserved tool gets -32601, never a backend fallthrough.
	if e, _ := rsp["1"]["error"].(map[string]any); e == nil || e["code"] != float64(-32601) {
		t.Fatalf("an approval tool call must be refused as unavailable: %v", rsp["1"])
	}
}

func TestLocalCommitPromptsAreAuditedEndToEnd(t *testing.T) {
	ownerDir := t.TempDir()
	fb := newCmdFakeBrowser()
	fb.target = &platform.TargetInfo{Resolved: true, Tag: "button", InputType: "submit", Name: "Send", Actionable: true, InForm: true, FormMethod: "post", Content: "typed-secret-body"}
	rt := newLocalCobrowseRuntime(localCobrowseOptions{ownerDir: func() string { return ownerDir }, browser: fb, policy: allowAllURLs})
	defer rt.close()
	one := func(line string) map[string]any {
		t.Helper()
		for _, v := range runLocalBridge(t, rt, nil, line) {
			return v
		}
		t.Fatalf("no response to %s", line)
		return nil
	}
	one(call(1, "local_cobrowse_start", map[string]any{"url": "https://fixture.test/", "goal": "send it"}))
	if err := rt.svc.ApproveDomain("cb-1", "fixture.test", 0); err != nil {
		t.Fatal(err)
	}
	resp := one(call(2, "local_cobrowse_click", map[string]any{"session_id": "cb-1", "selector": "#send"}))
	content, isErr := toolResult(t, resp)
	if !isErr || !strings.Contains(firstText(content), "commit_approval_required") {
		t.Fatalf("Send after a grant must prompt: %v", resp)
	}
	if fb.clicks != 0 {
		t.Fatal("the click reached the browser")
	}
	commit := rt.svc.Sessions("cb-1")[0].Commit
	if commit == nil {
		t.Fatal("no pending commit")
	}
	if err := rt.svc.ApproveCommit("cb-1", commit.ID); err != nil { // the viewer's action
		t.Fatal(err)
	}
	if _, isErr := toolResult(t, one(call(3, "local_cobrowse_click", map[string]any{"session_id": "cb-1", "selector": "#send"}))); isErr {
		t.Fatal("approved action must run on repeat")
	}
	if fb.clicks != 1 {
		t.Fatalf("clicks = %d", fb.clicks)
	}

	dir := filepath.Join(ownerDir, localaudit.DirName)
	recs, err := localaudit.Read(dir, localaudit.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var prompt, approve, ran bool
	for _, r := range recs {
		switch {
		case r.IntendedAction == "commit_prompt" && r.Domain == "fixture.test" && r.Fingerprint != "" && r.Label == "Send" && r.Rules != "":
			prompt = true
		case r.IntendedAction == "approve_commit" && r.Actor == "user" && r.Fingerprint != "":
			approve = true
		case r.IntendedAction == "click" && r.ResultingAction == "ok" && r.Fingerprint != "":
			ran = true
		}
	}
	if !prompt || !approve || !ran {
		t.Fatalf("prompt, approval and execution must all be audited: %v %v %v", prompt, approve, ran)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	for _, f := range files {
		if b, _ := os.ReadFile(f); bytes.Contains(b, []byte("typed-secret-body")) {
			t.Fatal("form content leaked into the audit log")
		}
	}
	var out bytes.Buffer
	if err := runLocalAudit(dir, localAuditOptions{verify: true}, &out); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

package localcobrowse

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// nodeDigests runs the page's own digest code (platform.FormDigestScript, the
// text embedded in the inspector) under Node for each ordered list of
// [name, id, type, value, checked] tuples. It skips where Node is absent.
func nodeDigests(t *testing.T, forms [][][]any) []string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; the real-browser job covers the inspector")
	}
	script := platform.FormDigestScript + `
const input = JSON.parse(require("fs").readFileSync(0, "utf8"));
process.stdout.write(JSON.stringify(input.map(formDigest)));`
	path := filepath.Join(t.TempDir(), "digest.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(forms)
	cmd := exec.Command(node, path)
	cmd.Stdin = bytes.NewReader(in)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("node: %v", err)
	}
	var digests []string
	if err := json.Unmarshal(out.Bytes(), &digests); err != nil || len(digests) != len(forms) {
		t.Fatalf("digests: %q %v", out.String(), err)
	}
	return digests
}

// Field snapshots that a "name=value" per line encoding cannot tell apart must
// give different digests, and an approval of one must not run the other.
func TestFormDigestIsUnambiguousAndApprovalDoesNotRedeemAcrossIt(t *testing.T) {
	tup := func(name, typ, value string, checked bool) []any { return []any{name, "", typ, value, checked} }
	pairs := map[string][2][][]any{
		"newline in a value vs a following field": {
			{tup("msg", "textarea", "hello\nrecipient=bob", false)},
			{tup("msg", "textarea", "hello", false), tup("recipient", "hidden", "bob", false)},
		},
		"equals sign in a name vs in a value": {
			{tup("a=b", "text", "c", false)},
			{tup("a", "text", "b=c", false)},
		},
		"newline then pair vs two text fields": {
			{tup("a", "text", "x\nb=y", false)},
			{tup("a", "text", "x", false), tup("b", "text", "y", false)},
		},
		"checkbox state": {
			{tup("agree", "checkbox", "on", true)},
			{tup("agree", "checkbox", "on", false)},
		},
	}
	for name, p := range pairs {
		t.Run(name, func(t *testing.T) {
			d := nodeDigests(t, [][][]any{p[0], p[1]})
			if d[0] == d[1] {
				t.Fatalf("distinct forms share the digest %s", d[0])
			}
			svc, fb, _, id := startGranted(t, nil)
			first := *sendTarget()
			first.ContentHash = d[0]
			second := first
			second.ContentHash = d[1]
			fb.target = &first
			if _, err := svc.Click(ctx, id, "#send", nil, nil, "g"); !errors.Is(err, ErrCommitApprovalRequired) {
				t.Fatalf("must ask: %v", err)
			}
			cv := pendingView(t, svc, id)
			fb.target = &second // the form changed after the prompt
			if err := svc.ApproveCommit(ctx, id, cv.ID); err != nil {
				t.Fatal(err)
			}
			if clicks(fb) != 0 {
				t.Fatal("the changed form ran under the earlier approval")
			}
			if r := svc.Sessions(id)[0].CommitResult; r == nil || r.State != "changed" {
				t.Fatalf("must resolve as changed: %+v", r)
			}
		})
	}
	// Identical forms still match (the digest is stable).
	same := tup("msg", "textarea", "hello", false)
	if d := nodeDigests(t, [][][]any{{same}, {same}}); d[0] != d[1] {
		t.Fatal("identical forms must have identical digests")
	}
}

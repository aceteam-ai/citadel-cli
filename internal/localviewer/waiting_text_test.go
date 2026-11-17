package localviewer

import (
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestWaitingBannerText runs the viewer's waitingText function under node (skipped
// when node is absent) so the banner wording is pinned without a browser.
func TestWaitingBannerText(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	src, err := assets.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)function waitingText\(reason\) \{.*?\r?\n  \}\r?\n`).Find(src)
	if m == nil {
		t.Fatal("waitingText not found in app.js")
	}
	cases := map[string]string{
		"sign in":      "Waiting for you: sign in. Then press Hand back to agent.",
		"sign in...  ": "Waiting for you: sign in. Then press Hand back to agent.",
		"sign in, then press Hand back to agent.":   "Waiting for you: sign in, then press Hand back to agent.",
		"Sign in then press HAND BACK to the agent": "Waiting for you: Sign in then press HAND BACK to the agent.",
	}
	for in, want := range cases {
		script := string(m) + "process.stdout.write(waitingText(" + strconv.Quote(in) + "));"
		out, err := exec.Command(node, "-e", script).Output()
		if err != nil {
			t.Fatalf("node: %v", err)
		}
		if got := string(out); got != want {
			t.Errorf("reason %q: got %q, want %q", in, got, want)
		}
		if strings.Contains(string(out), "..") {
			t.Errorf("double period in %q", out)
		}
	}
}

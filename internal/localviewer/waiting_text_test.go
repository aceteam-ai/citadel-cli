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
		"Hand control back when finished":           "Waiting for you: Hand control back when finished.",
		"Please hand the session back after login":  "Waiting for you: Please hand the session back after login.",
		"Sign in, then hand it all back":            "Waiting for you: Sign in, then hand it all back.",
		"Handle the login; back soon":               "Waiting for you: Handle the login; back soon. Then press Hand back to agent.",
		"Log in beforehand, then go back":           "Waiting for you: Log in beforehand, then go back. Then press Hand back to agent.",
		"Hand over the backup code":                 "Waiting for you: Hand over the backup code. Then press Hand back to agent.",
		"Write the code by hand, then type it back": "Waiting for you: Write the code by hand, then type it back. Then press Hand back to agent.",
		"Check the secondhand go back":              "Waiting for you: Check the secondhand go back. Then press Hand back to agent.",
		"Fill the form by hand one more time back":  "Waiting for you: Fill the form by hand one more time back. Then press Hand back to agent.",
		"Fill it in by hand and come back":          "Waiting for you: Fill it in by hand and come back. Then press Hand back to agent.",
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

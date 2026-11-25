package laneop

import "testing"

// Synthetic fixtures for the recognizer. They pin layout only; native
// acceptance against each live harness is a post-merge gate.
func TestReadScreenFixtures(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		kind  ScreenKind
		draft string
		busy  bool
	}{
		{"rule frame empty", "output\n" + rule + "\n> \n" + rule + "\n  ? for shortcuts\n\n\n", ScreenPrompt, "", false},
		{"rule frame with draft", rule + "\n❯ fix the flaky test\n" + rule + "\n", ScreenPrompt, "fix the flaky test", false},
		{"box frame empty", "╭──────────────────────╮\n│ >                    │\n╰──────────────────────╯\n", ScreenPrompt, "", false},
		{"box frame multi-line draft", "╭──────────────────────╮\n│ > first              │\n│   second             │\n╰──────────────────────╯\n", ScreenPrompt, "first\nsecond", false},
		{"busy with empty box", "✻ Thinking… (esc to interrupt)\n" + rule + "\n> \n" + rule + "\n", ScreenPrompt, "", true},
		{"nbsp after glyph", rule + "\n> \n" + rule + "\n", ScreenPrompt, "", false},
		{"transcript prompt without a frame", "> an old prompt\nanswer text\n", ScreenUnknown, "", false},
		{"shell prompt", "user@host:~/src$ ", ScreenUnknown, "", false},
		{"glyph not followed by space", rule + "\n>>> not a prompt\n" + rule + "\n", ScreenUnknown, "", false},
		{"frame too far above footer", rule + "\n> \n" + rule + "\n1\n2\n3\n4\n5\n6\n7\n", ScreenUnknown, "", false},
		{"numbered dialog", " Do you want to proceed?\n ❯ 1. Yes\n   2. No, tell Claude what to do\n", ScreenDialog, "", false},
		{"selector without question", "│ › 1. Approve\n│   2. Deny\n" + rule + "\n> \n" + rule + "\n", ScreenDialog, "", false},
		{"empty", "", ScreenUnknown, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ReadScreen(tc.text)
			if got.Kind != tc.kind || got.Draft != tc.draft || got.Busy != tc.busy {
				t.Fatalf("got %+v, want kind=%d draft=%q busy=%v", got, tc.kind, tc.draft, tc.busy)
			}
		})
	}
}

func TestDraftHolds(t *testing.T) {
	if !draftHolds("first\nsecond", "first second") || !draftHolds("[Pasted text #2 +14 lines]", "anything") {
		t.Fatal("expected match")
	}
	if draftHolds("", "") || draftHolds("prefix first", "first") || draftHolds("[Pasted text #2] extra", "x") {
		t.Fatal("unexpected match")
	}
}

func TestScrubMergesOverlaps(t *testing.T) {
	key := "ghp_" + "0123456789abcdefghijklmnopqrstuvwxyz"
	got := Scrub("a " + key + " b access_token=" + key)
	if got != "a [REDACTED] b [REDACTED]" && got != "a [REDACTED] b access_token=[REDACTED]" {
		t.Fatalf("scrub %q", got)
	}
}

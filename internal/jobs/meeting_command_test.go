package jobs

import "testing"

func TestParseCommand_LeaveVariants(t *testing.T) {
	// The one auto-executed command must be recognized across casing,
	// punctuation, and surrounding words — but only via the literal /ace token.
	cases := []string{
		"/ace leave",
		"/ACE Leave",
		"  /ace   leave  ",
		"/ace: leave",
		"/ace leave.",
		"okay everyone, /ace leave now please",
		"/Ace leave the call",
	}
	for _, in := range cases {
		got, ok := ParseCommand(in, SourceTranscript)
		if !ok {
			t.Errorf("ParseCommand(%q) not recognized, want leave", in)
			continue
		}
		if got.Kind != CommandLeave {
			t.Errorf("ParseCommand(%q).Kind = %q, want %q", in, got.Kind, CommandLeave)
		}
		if got.Command != "leave" {
			t.Errorf("ParseCommand(%q).Command = %q, want leave", in, got.Command)
		}
		if got.Raw != in {
			t.Errorf("ParseCommand(%q).Raw = %q, want the original line", in, got.Raw)
		}
	}
}

func TestParseCommand_GenericCapture(t *testing.T) {
	// A /ace command not in the registry is captured (not executed) with its
	// arguments preserved as the instruction.
	got, ok := ParseCommand("/ace summarize the last five minutes", SourceChat)
	if !ok {
		t.Fatal("expected generic command to be recognized")
	}
	if got.Kind != CommandGeneric {
		t.Errorf("Kind = %q, want %q", got.Kind, CommandGeneric)
	}
	if got.Command != "summarize" {
		t.Errorf("Command = %q, want summarize", got.Command)
	}
	if got.Instruction != "the last five minutes" {
		t.Errorf("Instruction = %q, want 'the last five minutes'", got.Instruction)
	}
	if got.Source != SourceChat {
		t.Errorf("Source = %q, want %q", got.Source, SourceChat)
	}
}

func TestParseCommand_WakePhrase(t *testing.T) {
	cases := map[string]string{
		"hey aceteam, summarize that":         "summarize that",
		"Hey AceTeam summarize that":          "summarize that",
		"so hey ace team, what did John say?": "what did John say?",
		"HEY ACETEAM: mute yourself":          "mute yourself",
	}
	for in, wantInstr := range cases {
		got, ok := ParseCommand(in, SourceTranscript)
		if !ok {
			t.Errorf("ParseCommand(%q) not recognized, want instruction", in)
			continue
		}
		if got.Kind != CommandInstruction {
			t.Errorf("ParseCommand(%q).Kind = %q, want %q", in, got.Kind, CommandInstruction)
		}
		if got.Instruction != wantInstr {
			t.Errorf("ParseCommand(%q).Instruction = %q, want %q", in, got.Instruction, wantInstr)
		}
	}
}

func TestParseCommand_SpokenSlashHeuristic(t *testing.T) {
	// The spoken-command heuristic ("slash ace leave") is best-effort; when it
	// resolves to a registry command it still classifies correctly.
	got, ok := ParseCommand("slash ace leave", SourceTranscript)
	if !ok {
		t.Fatal("expected spoken slash-ace to be recognized")
	}
	if got.Kind != CommandLeave {
		t.Errorf("Kind = %q, want %q", got.Kind, CommandLeave)
	}
}

func TestParseCommand_NonMatches(t *testing.T) {
	// Ambient speech and near-misses must NOT trigger a command — especially not
	// the destructive leave. This is the safety property.
	nonMatches := []string{
		"",
		"   ",
		"does the ace leave early?",
		"we should ace this presentation",
		"the interface looks great",
		"hey team, let's start",       // not the wake phrase
		"hey aceteam",                 // bare wake phrase, no instruction
		"hey aceteam,   ",             // wake phrase, empty instruction
		"/ace",                        // bare token, no command word
		"/ace   ",                     // token with only whitespace
		"please leave the meeting",    // 'leave' without the /ace token
		"my email is a/ace@thing.com", // no space-delimited command word
	}
	for _, in := range nonMatches {
		if got, ok := ParseCommand(in, SourceTranscript); ok {
			t.Errorf("ParseCommand(%q) = %+v, ok=true; want no match", in, got)
		}
	}
}

func TestParseCommand_HallucinatedPartialSegment(t *testing.T) {
	// A garbled/partial transcript segment must not falsely match. Only a clean
	// /ace token or wake phrase counts.
	partials := []string{
		"a-ace lea",
		"...ace lev the ca",
		"h y acetea",
	}
	for _, in := range partials {
		if _, ok := ParseCommand(in, SourceTranscript); ok {
			t.Errorf("ParseCommand(%q) matched a partial/hallucinated segment; want no match", in)
		}
	}
}

func TestParseCommand_PickFirst(t *testing.T) {
	// A line containing more than one invocation resolves to the first (the
	// literal /ace token, which outranks the wake phrase) — never ambiguous.
	got, ok := ParseCommand("/ace leave and hey aceteam summarize", SourceTranscript)
	if !ok {
		t.Fatal("expected a match")
	}
	if got.Kind != CommandLeave {
		t.Errorf("Kind = %q, want leave (literal token outranks wake phrase)", got.Kind)
	}
}

func TestParseCommand_RegistryExtensible(t *testing.T) {
	// Guard the registry contract: "leave" is the only auto-executed command
	// today. If this changes, the wiring in meeting_join.go must be reviewed for
	// what new destructive actions become auto-executable.
	if len(commandRegistry) != 1 {
		t.Fatalf("commandRegistry has %d entries; adding auto-executed commands requires reviewing meeting_join.go wiring", len(commandRegistry))
	}
	if commandRegistry["leave"] != CommandLeave {
		t.Errorf("registry[leave] = %q, want %q", commandRegistry["leave"], CommandLeave)
	}
}

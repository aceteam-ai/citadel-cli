package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTranscriptSummary_SelectsSemanticTextOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	data := strings.Join([]string{
		`{"type":"progress","message":{"role":"system","content":"internal"}}`,
		`{"type":"user","message":{"role":"user","content":"  Please   remember this. "}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","text":"secret chain"},{"type":"tool_use","text":"raw tool"},{"type":"text","text":"The deployment uses port 443."}]}}`,
		`{"type":"user","isMeta":true,"message":{"role":"user","content":"meta text"}}`,
		`not json`,
	}, "\n")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	got := TranscriptSummary(path, 500)
	if got != "User: Please remember this.\nAssistant: The deployment uses port 443." {
		t.Fatalf("unexpected summary: %q", got)
	}
	for _, forbidden := range []string{"secret chain", "raw tool", "meta text", "not json", `"role"`} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("summary leaked non-semantic content %q: %q", forbidden, got)
		}
	}
}

func TestTranscriptSummary_BoundedAtRuneBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	line := `{"type":"assistant","message":{"role":"assistant","content":"hello 🚀 world and more"}}`
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	got := TranscriptSummary(path, 12)
	if len([]rune(got)) != 12 || !strings.HasSuffix(got, "…") || strings.ContainsRune(got, '�') {
		t.Fatalf("summary was not rune-bounded cleanly: %q (%d runes)", got, len([]rune(got)))
	}
}

func TestStableCaptureName_IsStableAndCollisionResistant(t *testing.T) {
	a := StableCaptureName("session-a", "/ignored", "ignored")
	if a != StableCaptureName("session-a", "/different", "different") {
		t.Fatalf("session identity was not stable: %q", a)
	}
	if a == StableCaptureName("session-b", "/ignored", "ignored") {
		t.Fatal("different sessions collided")
	}
	if !strings.HasPrefix(a, "claude-session-") || len(a) != len("claude-session-")+20 {
		t.Fatalf("unexpected name format: %q", a)
	}
	if strings.Contains(a, "session-a") {
		t.Fatal("session id leaked in memory name")
	}
}

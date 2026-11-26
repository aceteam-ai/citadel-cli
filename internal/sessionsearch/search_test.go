//go:build linux || darwin

package sessionsearch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/aceteam-ai/citadel-cli/internal/laneinventory"
)

const testID = "11111111-2222-4333-8444-555555555555"
const testOtherID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

func fixture(t *testing.T, home, path, body string) {
	t.Helper()
	path = filepath.Join(home, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
func find(t *testing.T, home string, opts Options) Report {
	t.Helper()
	report, err := Search(context.Background(), home, os.Getuid(), opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	return report
}
func TestSessionSearchPromptReplySelection(t *testing.T) {
	home := t.TempDir()
	fixture(t, home, ".claude/history.jsonl", `{"display":"violet compass prompt","sessionId":"`+testID+`","timestamp":1793491200000,"pastedContents":{"text":"planted paste"}}`+"\n")
	fixture(t, home, ".claude/projects/project/"+testID+".jsonl", strings.Join([]string{
		`{"type":"custom-title","customTitle":"Compass session"}`,
		`{"type":"assistant","timestamp":"2026-11-01T00:01:00Z","message":{"role":"assistant","content":[{"type":"text","text":"violet compass reply"},{"type":"tool_use","text":"planted tool"},{"type":"thinking","text":"planted thought"}]}}`,
		`{"type":"user","timestamp":"2026-11-01T00:02:00Z","message":{"role":"user","content":[{"type":"text","text":"planted injected"}]}}`,
		`{"type":"assistant","isMeta":true,"timestamp":"2026-11-01T00:03:00Z","message":{"role":"assistant","content":[{"type":"text","text":"planted meta"}]}}`,
		`{"type":"assistant","isSidechain":true,"timestamp":"2026-11-01T00:04:00Z","message":{"role":"assistant","content":[{"type":"text","text":"planted sidechain"}]}}`,
	}, "\n")+"\n")
	r := find(t, home, Options{Query: "violet compass"})
	if len(r.Hits) != 1 || len(r.Hits[0].Snippets) != 2 || r.Hits[0].Title != "Compass session" || r.Hits[0].State != "ended" || r.Hits[0].Open != "claude -r "+testID {
		t.Fatalf("unexpected result: %+v", r)
	}
	if r.Hits[0].Snippets[0].Kind != "reply" || r.Hits[0].Snippets[1].Kind != "prompt" {
		t.Fatal("missing source labels")
	}
	if r := find(t, home, Options{Query: "planted"}); len(r.Hits) != 0 {
		t.Fatal("excluded content searchable")
	}
}
func TestSessionSearchCodexAndFilters(t *testing.T) {
	home := t.TempDir()
	fixture(t, home, ".codex/history.jsonl", `{"text":"violet prompt","session_id":"`+testID+`","ts":1793491200}`+"\n")
	fixture(t, home, ".codex/sessions/2026/11/01/rollout-2026-11-01T00-00-00-"+testID+".jsonl", strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"` + testID + `","cwd":"/synthetic/project"}}`,
		`{"type":"response_item","timestamp":"2026-11-01T00:02:00Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"violet reply"},{"type":"input_text","text":"excluded input"}]}}`,
		`{"type":"response_item","timestamp":"2026-11-01T00:02:00Z","payload":{"type":"function_call_output","role":"assistant","content":[{"type":"output_text","text":"excluded tool"}]}}`,
		`{"type":"event_msg","timestamp":"2026-11-01T00:02:00Z","payload":{"type":"agent_message","message":"excluded event"}}`,
	}, "\n")+"\n")
	r := find(t, home, Options{Query: "violet", Harness: "codex", Since: time.Date(2026, 11, 1, 0, 1, 0, 0, time.UTC)})
	if len(r.Hits) != 1 || len(r.Hits[0].Snippets) != 1 || r.Hits[0].Snippets[0].Kind != "reply" || r.Hits[0].Open != "codex resume "+testID {
		t.Fatalf("unexpected result: %+v", r)
	}
	if r.Hits[0].Cwd != "/synthetic/project" {
		t.Fatal("session metadata directory missing")
	}
	if r := find(t, home, Options{Query: "excluded"}); len(r.Hits) != 0 {
		t.Fatal("non-assistant content searchable")
	}
	if r := find(t, home, Options{Query: "violet", Harness: "claude"}); len(r.Hits) != 0 {
		t.Fatal("harness filter ignored")
	}
}
func TestSessionSearchWithholdAndBounds(t *testing.T) {
	home := t.TempDir()
	secret := "act_" + strings.Repeat("z", 32)
	var lines []string
	for i := 0; i < 5; i++ {
		lines = append(lines, fmt.Sprintf(`{"display":%q,"sessionId":%q,"timestamp":1793491200000}`, "needle "+strings.Repeat("x", 500)+" "+secret, testID))
	}
	fixture(t, home, ".claude/history.jsonl", strings.Join(lines, "\n"))
	r := find(t, home, Options{Query: "needle"})
	if len(r.Hits) != 1 || len(r.Hits[0].Snippets) != 3 || r.Hits[0].State != "pruned" || r.Hits[0].Open != "" {
		t.Fatalf("unexpected result: %+v", r)
	}
	for _, snippet := range r.Hits[0].Snippets {
		if snippet.Text != withheld {
			t.Fatal("secret not withheld before clipping")
		}
	}
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), secret) {
		t.Fatal("secret serialized")
	}
	if got := safe("hello\x1b]52;clipboard\a\nworld"); strings.ContainsAny(got, "\x1b\a\n") {
		t.Fatal("terminal controls survived")
	}
	if len([]rune(safe(strings.Repeat("界", 300)))) != 240 {
		t.Fatal("rune bound changed")
	}
}

func TestSessionSearchDisplayFormatCharacters(t *testing.T) {
	home := t.TempDir()
	fixture(t, home, ".claude/history.jsonl", `{"display":"needle prompt\u202e next\u200b field","project":"/synthetic/\u2066project\u2069","sessionId":"`+testID+`","timestamp":1793491200000}`+"\n")
	fixture(t, home, ".claude/projects/project/"+testID+".jsonl", `{"type":"custom-title","customTitle":"title\u202e next\u202c\u200d field"}`+"\n"+
		`{"type":"assistant","timestamp":"2026-11-01T00:02:00Z","message":{"role":"assistant","content":[{"type":"text","text":"needle reply\u2067 next\u2069\ufeff field"}]}}`+"\n")
	report := find(t, home, Options{Query: "needle"})
	if len(report.Hits) != 1 || len(report.Hits[0].Snippets) != 2 {
		t.Fatal("synthetic display fixture not selected")
	}
	hit := report.Hits[0]
	values := []string{hit.Title, hit.Cwd}
	for _, snippet := range hit.Snippets {
		values = append(values, snippet.Text)
	}
	for _, value := range values {
		if value == "" {
			t.Fatal("display value missing")
		}
		for _, r := range value {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				t.Fatal("display formatting character survived")
			}
		}
	}
	if hit.Title != "title next field" || hit.Cwd != "/synthetic/ project" {
		t.Fatalf("unexpected normalized display: %q %q", hit.Title, hit.Cwd)
	}
	for _, r := range []rune{'\u202a', '\u202b', '\u202c', '\u202d', '\u202e', '\u2066', '\u2067', '\u2068', '\u2069', '\u200b', '\u200c', '\u200d', '\u200e', '\u200f', '\u2060', '\ufeff'} {
		if got := safe("before" + string(r) + "after"); got != "before after" {
			t.Fatalf("format category U+%04X survived", r)
		}
	}
}

func TestSessionSearchNormalizedValueWithholding(t *testing.T) {
	// Synthetic detector fixtures, not credentials. Every display projection
	// checks the normalized full value even when the matching snippet is earlier.
	value := "api_key\u200b=" + strings.Repeat("A", 20)
	for _, separator := range []string{"\u200b", "\u2066", "\x01", "\n"} {
		if got := safe("api_key" + separator + "=" + strings.Repeat("A", 20)); got != withheld {
			t.Fatal("normalized detector fixture was not withheld")
		}
	}
	home := t.TempDir()
	row, _ := json.Marshal(map[string]any{"display": "needle " + strings.Repeat("x", 300) + " " + value, "project": "/synthetic/" + value, "sessionId": testID, "timestamp": int64(1793491200000)})
	fixture(t, home, ".claude/history.jsonl", string(row)+"\n")
	title, _ := json.Marshal(map[string]any{"type": "custom-title", "customTitle": value})
	fixture(t, home, ".claude/projects/project/"+testID+".jsonl", string(title)+"\n")
	report := find(t, home, Options{Query: "needle"})
	if len(report.Hits) != 1 || len(report.Hits[0].Snippets) != 1 {
		t.Fatal("normalized display fixture not selected")
	}
	hit := report.Hits[0]
	if hit.Title != withheld || hit.Cwd != withheld || hit.Snippets[0].Text != withheld {
		t.Fatal("normalized full-value guard missing from a display projection")
	}
}
func TestSessionSearchPlacement(t *testing.T) {
	r := Report{Hits: []Hit{{ID: testID, Harness: "claude", State: "ended"}}}
	p := laneinventory.Pane{ThreadID: testID, Harness: "claude", Target: "%8", DeclaredSession: "sample-session"}
	Locate(&r, []laneinventory.Pane{p})
	if r.Hits[0].Open != "citadel attach sample-session" || r.Hits[0].State != "running" {
		t.Fatal("declared binding absent")
	}
	r.Hits[0].Open = ""
	r.Hits[0].State = "ended"
	Locate(&r, []laneinventory.Pane{p, p})
	if r.Hits[0].Open != "" {
		t.Fatal("ambiguous binding selected")
	}
	p.Target = "%8; invalid"
	Locate(&r, []laneinventory.Pane{p})
	if r.Hits[0].Open != "" {
		t.Fatal("unvalidated target selected")
	}
}
func TestSessionSearchFileBoundariesAndCancellation(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	fixture(t, outside, "history.jsonl", `{"display":"needle","sessionId":"`+testID+`","timestamp":1793491200000}`)
	if err := os.Symlink(outside, filepath.Join(home, ".claude")); err != nil {
		t.Fatal(err)
	}
	r := find(t, home, Options{Query: "needle"})
	if len(r.Hits) != 0 || !r.Partial {
		t.Fatal("linked source admitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err := Search(ctx, t.TempDir(), os.Getuid(), Options{Query: "needle"}, nil)
	if err != nil || !r.Partial {
		t.Fatal("cancel not reported")
	}
	if _, err := Search(context.Background(), home, os.Getuid()+1, Options{Query: "needle"}, nil); err != ErrUnavailable {
		t.Fatal("foreign owner accepted")
	}
	for _, opts := range []Options{{}, {Query: "needle", Harness: "other"}, {Query: strings.Repeat("x", 1025)}} {
		if _, err := Search(context.Background(), home, os.Getuid(), opts, nil); err != ErrOptions {
			t.Fatal("invalid options accepted")
		}
	}
}
func TestSessionSearchMalformedRecordDoesNotDiscardOthers(t *testing.T) {
	home := t.TempDir()
	fixture(t, home, ".claude/history.jsonl", "{malformed}\n"+`{"display":"needle","sessionId":"`+testID+`","timestamp":1793491200000}`+"\n")
	r := find(t, home, Options{Query: "needle"})
	if len(r.Hits) != 1 || !r.Partial {
		t.Fatal("malformed line discarded valid record or hid incomplete coverage")
	}
}

func TestSessionSearchMalformedOptionalBlockAndTitle(t *testing.T) {
	home := t.TempDir()
	fixture(t, home, ".claude/history.jsonl", `{"display":"ordinary prompt","sessionId":"`+testID+`","timestamp":1793491200000}`+"\n")
	fixture(t, home, ".claude/projects/project/"+testID+".jsonl", `{"type":"custom-title","customTitle":"violet compass"}`+"\n"+
		`{"type":"assistant","timestamp":"2026-11-01T00:02:00Z","message":{"role":"assistant","content":[{"type":"tool_use","text":123},{"type":"text","text":"needle reply"}]}}`+"\n")
	if r := find(t, home, Options{Query: "violet compass"}); len(r.Hits) != 1 {
		t.Fatal("title not searchable")
	}
	r := find(t, home, Options{Query: "needle"})
	if len(r.Hits) != 1 || len(r.Hits[0].Snippets) != 1 || !r.Partial {
		t.Fatal("malformed optional block discarded valid reply")
	}
}

func TestSessionSearchResultLimitAndDateRange(t *testing.T) {
	home := t.TempDir()
	var lines []string
	for i := 0; i < 25; i++ {
		lines = append(lines, fmt.Sprintf(`{"display":"needle","sessionId":"11111111-2222-4333-8444-%012d","timestamp":%d}`, i, int64(1793491200000)+int64(i)*1000))
	}
	fixture(t, home, ".claude/history.jsonl", strings.Join(lines, "\n"))
	r := find(t, home, Options{Query: "needle"})
	if len(r.Hits) != 20 || !r.Partial || !strings.HasSuffix(r.Hits[0].ID, "000000000024") {
		t.Fatal("result limit or recent ordering lost")
	}
	r = find(t, home, Options{Query: "needle", Until: time.UnixMilli(1793491201000)})
	if len(r.Hits) != 2 || r.Partial {
		t.Fatal("inclusive until filter lost")
	}
}

func TestSessionSearchOversizedLineAndNonRegularSource(t *testing.T) {
	home := t.TempDir()
	fixture(t, home, ".claude/history.jsonl", strings.Repeat("x", maxLineBytes+1))
	r := find(t, home, Options{Query: "needle"})
	if !r.Partial || len(r.Hits) != 0 {
		t.Fatal("oversized record not bounded")
	}
	other := t.TempDir()
	if err := os.MkdirAll(filepath.Join(other, ".claude", "history.jsonl"), 0700); err != nil {
		t.Fatal(err)
	}
	r = find(t, other, Options{Query: "needle"})
	if !r.Partial {
		t.Fatal("non-regular content source accepted")
	}
}

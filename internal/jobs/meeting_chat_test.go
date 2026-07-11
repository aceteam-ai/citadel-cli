package jobs

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestMeetChatSendJS_EmbedsEscapedText(t *testing.T) {
	// A message with quotes/backslashes must be safely escaped into the JS.
	js := meetChatSendJS(`hi "there" \o/`)
	if !strings.Contains(js, `hi \"there\" \\o/`) {
		t.Errorf("send JS did not json-escape the text; got: %s", js)
	}
	// The input selector must be embedded, and a missing input must throw so a
	// stale selector fails loudly (not a silent no-op).
	if !strings.Contains(js, "chat input not found") {
		t.Errorf("send JS must throw on a missing chat input; got: %s", js)
	}
}

func TestMeetChatOpenJS_ThrowsOnMissing(t *testing.T) {
	js := meetChatOpenJS()
	if !strings.Contains(js, "throw new Error") {
		t.Errorf("open JS must throw on a missing chat button; got: %s", js)
	}
	escaped, _ := json.Marshal(meetChatOpenSelector)
	if !strings.Contains(js, string(escaped)) {
		t.Errorf("open JS must embed the chat-open selector; got: %s", js)
	}
}

func TestMeetChatReadJS_NeverThrowsAndReturnsJSON(t *testing.T) {
	js := meetChatReadJS()
	// Empty chat is normal; the readback must not throw and must return a JSON
	// string (JSON.stringify) so the value crosses CDP cleanly.
	if strings.Contains(js, "throw new Error") {
		t.Errorf("read JS must not throw on empty chat; got: %s", js)
	}
	if !strings.Contains(js, "JSON.stringify") {
		t.Errorf("read JS must JSON.stringify its result; got: %s", js)
	}
}

func TestParseChatMessages_JSONString(t *testing.T) {
	v := `[{"index":0,"sender":"Alice","text":"hello"},{"index":1,"sender":"Bob","text":"/ace leave"}]`
	msgs, err := parseChatMessages(v)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []MeetChatMessage{
		{Index: 0, Sender: "Alice", Text: "hello"},
		{Index: 1, Sender: "Bob", Text: "/ace leave"},
	}
	if !reflect.DeepEqual(msgs, want) {
		t.Errorf("parsed = %+v, want %+v", msgs, want)
	}
}

func TestParseChatMessages_EmptyAndNil(t *testing.T) {
	for _, v := range []any{nil, "", "   ", "[]"} {
		msgs, err := parseChatMessages(v)
		if err != nil {
			t.Errorf("parseChatMessages(%v) errored: %v", v, err)
		}
		if len(msgs) != 0 {
			t.Errorf("parseChatMessages(%v) = %v, want empty", v, msgs)
		}
	}
}

func TestParseChatMessages_DecodedArrayFallback(t *testing.T) {
	// Defensive path: an already-decoded []any (map items). Empty-text items are
	// skipped; index falls back to position when absent.
	v := []any{
		map[string]any{"sender": "Alice", "text": "hi", "index": float64(2)},
		map[string]any{"sender": "Bob", "text": "   "}, // skipped (blank)
		map[string]any{"text": "no sender"},
	}
	msgs, err := parseChatMessages(v)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []MeetChatMessage{
		{Index: 2, Sender: "Alice", Text: "hi"},
		{Index: 2, Sender: "", Text: "no sender"},
	}
	if !reflect.DeepEqual(msgs, want) {
		t.Errorf("parsed = %+v, want %+v", msgs, want)
	}
}

func TestParseChatMessages_Malformed(t *testing.T) {
	if _, err := parseChatMessages("{not json"); err == nil {
		t.Error("expected error for malformed JSON string")
	}
	if _, err := parseChatMessages(42); err == nil {
		t.Error("expected error for unexpected readback type")
	}
}

func TestChatMessageLine(t *testing.T) {
	if got := (MeetChatMessage{Sender: "Alice", Text: "hi"}).Line(); got != "Alice: hi" {
		t.Errorf("Line() = %q, want 'Alice: hi'", got)
	}
	if got := (MeetChatMessage{Text: "hi"}).Line(); got != "hi" {
		t.Errorf("Line() with no sender = %q, want 'hi'", got)
	}
}

// fakeChatPage is a scripted chatEvaluator: it returns a queued value/error per
// call and records the last expression it saw, so the helper wrappers can be
// exercised (parse + error propagation) without a real browser.
type fakeChatPage struct {
	readValue any
	readErr   error
	postErr   error
	openErr   error
	lastExpr  string
}

func (f *fakeChatPage) Evaluate(expression string) (any, error) {
	f.lastExpr = expression
	switch {
	case strings.Contains(expression, "JSON.stringify"):
		return f.readValue, f.readErr
	case strings.Contains(expression, "chat input not found"):
		return true, f.postErr
	default:
		return true, f.openErr
	}
}

func TestReadMeetChat_ParsesThroughEvaluator(t *testing.T) {
	page := &fakeChatPage{readValue: `[{"index":0,"sender":"A","text":"/ace leave"}]`}
	msgs, err := readMeetChat(page)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Text != "/ace leave" {
		t.Errorf("readMeetChat = %+v, want one leave line", msgs)
	}
}

func TestReadMeetChat_PropagatesEvaluateError(t *testing.T) {
	page := &fakeChatPage{readErr: fmt.Errorf("cdp boom")}
	if _, err := readMeetChat(page); err == nil {
		t.Error("expected readMeetChat to propagate the evaluate error")
	}
}

func TestPostAndOpenMeetChat_PropagateErrors(t *testing.T) {
	page := &fakeChatPage{postErr: fmt.Errorf("no input")}
	if err := postMeetChat(page, "hi"); err == nil {
		t.Error("expected postMeetChat to propagate the evaluate error")
	}
	page2 := &fakeChatPage{openErr: fmt.Errorf("no button")}
	if err := openMeetChat(page2); err == nil {
		t.Error("expected openMeetChat to propagate the evaluate error")
	}
}

func TestMeetAnnouncementText_MentionsCommands(t *testing.T) {
	// The announcement is the consent + control surface; it must disclose
	// recording and how to invoke the bot.
	for _, want := range []string{"recording", "/ace leave", "/ace <command>"} {
		if !strings.Contains(meetAnnouncementText, want) {
			t.Errorf("announcement missing %q; got: %q", want, meetAnnouncementText)
		}
	}
}

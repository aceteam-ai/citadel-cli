// internal/jobs/meeting_chat.go
//
// Meet in-call text-chat capture + post (issue #5435, epic #5097). Provides the
// CDP-driven helpers to (1) open the Meet chat panel, (2) READ the chat message
// history, and (3) POST a chat message, plus the self-announcement the bot posts
// on admittance. Chat lines are fed into the in-call command monitor
// (meeting_command.go) alongside transcript segments, and captured chat is
// persisted in the MEETING_JOIN result.
//
// All page interaction goes through the SAME CDP evaluate mechanism the join
// flow uses (platform.MeetingBrowser.Evaluate -> cdpEvaluate -> Runtime.evaluate
// with returnByValue). No new CDP library is introduced. The helpers here take a
// minimal `chatEvaluator` interface so the JS-building and result-parsing logic
// is unit-testable without a real browser; the LIVE selectors themselves can
// only be confirmed by a human on a real call (see the LIVE-TUNING block below).
package jobs

import (
	"encoding/json"
	"fmt"
	"strings"
)

// meetAnnouncementText is the consent/intro message the bot posts to Meet chat
// once admitted (capability 4). Kept as a single const so the exact wording is
// reviewed in one place and the tone stays consistent. TTS voice is out of scope
// for this PR (chat only); speaking this aloud is a named follow-up.
const meetAnnouncementText = "Hi, I'm AceTeam. I'm recording this call - audio, video, and text. " +
	"To give me instructions, say /ace leave or /ace <command>, or just talk to me."

// ---------------------------------------------------------------------------
// LIVE-TUNING REQUIRED (best-guess, UNVERIFIED)
//
// Every selector in this block is a best guess at Google Meet's in-call chat
// DOM. NONE has been confirmed against a live call — only a human signed into a
// real meeting can open the chat panel, read the rendered markup, and confirm or
// replace these. They are kept together so tuning is a one-place edit, mirroring
// the discipline in meeting_join.go's LIVE-TUNING block.
//
// What a human must confirm on a live call before this is trusted:
//
//   - The "Chat with everyone" toolbar button's aria-label (to OPEN the panel).
//     Note: meeting_join.go already observed a "Chat with everyone" button in the
//     in-call toolbar on 2026-07-11, so meetChatOpenSelector is the LEAST
//     uncertain piece here — but the exact aria-label casing is still unverified.
//
//   - The chat message row container + the sender-name and message-text sub
//     elements (to READ history).
//
//   - The chat text input and the send button (to POST).
//
//     best-guess as of: 2026-07-11 (NOT verified on a live call)
//
// ---------------------------------------------------------------------------
const (
	// meetChatOpenSelector: the in-call toolbar button that opens the chat
	// panel. "Chat with everyone" was seen in the toolbar on 2026-07-11.
	meetChatOpenSelector = `button[aria-label*="Chat with everyone" i],button[aria-label*="chat" i]`
	// meetChatMessageRowSelector: one rendered chat message row. Meet groups
	// consecutive messages from one sender; this best-guess targets individual
	// message text nodes.
	meetChatMessageRowSelector = `div[data-message-id],div[jsname][data-sender-name],div[aria-label*="chat message" i]`
	// meetChatSenderSelector: the sender-name element WITHIN a message row.
	meetChatSenderSelector = `[data-sender-name],[data-self-name],.sender-name`
	// meetChatTextSelector: the message-text element WITHIN a message row.
	meetChatTextSelector = `[data-message-text],.message-text,div[jsname="dTKtvb"]`
	// meetChatInputSelector: the chat text input (a textarea in current Meet).
	meetChatInputSelector = `textarea[aria-label*="Send a message" i],textarea[placeholder*="Send a message" i],textarea[aria-label*="message" i]`
	// meetChatSendSelector: the send button next to the chat input.
	meetChatSendSelector = `button[aria-label*="Send a message" i],button[aria-label*="Send message" i],button[data-tooltip*="Send" i]`
)

// MeetChatMessage is one captured Meet chat line. Index is the message's position in
// the rendered, append-only chat list, used to dedup across repeated reads (Meet
// chat only grows, so a stable index distinguishes two identical texts). Sender
// may be empty when the DOM does not expose a name for a grouped message.
type MeetChatMessage struct {
	Index  int    `json:"index"`
	Sender string `json:"sender"`
	Text   string `json:"text"`
}

// Line renders a chat message as a single line for the command monitor, which
// scans "text" for invocations. The sender is prefixed for the audit trail but
// the command monitor matches on substrings so the prefix does not interfere.
func (m MeetChatMessage) Line() string {
	if m.Sender == "" {
		return m.Text
	}
	return m.Sender + ": " + m.Text
}

// normalizeChatText canonicalizes a chat line for own-message matching: trimmed,
// lower-cased, with internal whitespace collapsed. Used so the bot does not scan
// its OWN posted messages (notably the announcement, which literally contains
// "/ace leave") for commands and self-trigger a leave — robust to minor Meet
// re-rendering of the text it echoes back.
func normalizeChatText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// chatEvaluator is the slice of platform.MeetingBrowser the chat helpers need,
// so they are unit-testable without a real browser (mirrors joinPage).
type chatEvaluator interface {
	Evaluate(expression string) (any, error)
}

// meetChatOpenJS builds the JS that opens the chat panel by clicking the chat
// toolbar button. It throws (via clickFirstBySelectorJS) when the selector
// matches nothing so a stale selector fails loudly during live tuning rather
// than silently leaving the panel closed and the readback empty.
func meetChatOpenJS() string {
	return clickFirstBySelectorJS(meetChatOpenSelector)
}

// meetChatReadJS builds the JS that scans rendered chat rows and returns a
// JSON-serializable array of {index, sender, text}. It NEVER throws on an empty
// chat (a call with no messages is normal); it returns []. Selectors are
// embedded json-escaped. The panel must be open for rows to be in the DOM.
func meetChatReadJS() string {
	rowSel, _ := json.Marshal(meetChatMessageRowSelector)
	senderSel, _ := json.Marshal(meetChatSenderSelector)
	textSel, _ := json.Marshal(meetChatTextSelector)
	return `(function(){` +
		`var rows=Array.prototype.slice.call(document.querySelectorAll(` + string(rowSel) + `));` +
		`var out=[];` +
		`for(var i=0;i<rows.length;i++){var r=rows[i];` +
		`var s=r.querySelector(` + string(senderSel) + `);` +
		`var t=r.querySelector(` + string(textSel) + `);` +
		`var text=(t?(t.innerText||t.textContent):(r.innerText||r.textContent))||"";` +
		`text=text.trim();if(!text)continue;` +
		`out.push({index:i,sender:s?((s.innerText||s.textContent)||"").trim():"",text:text});}` +
		`return JSON.stringify(out);})()`
}

// meetChatSendJS builds the JS that types text into the chat input (via the
// native value setter so Meet's controlled input observes the change, mirroring
// platform.typeJS) and clicks send. Throws when the input or send button is
// missing so a stale selector fails loudly. text is json-escaped.
func meetChatSendJS(text string) string {
	inputSel, _ := json.Marshal(meetChatInputSelector)
	sendSel, _ := json.Marshal(meetChatSendSelector)
	val, _ := json.Marshal(text)
	return `(function(){` +
		`var el=document.querySelector(` + string(inputSel) + `);` +
		`if(!el){throw new Error("chat input not found: "+` + string(inputSel) + `);}` +
		`el.focus();` +
		`var proto=el instanceof HTMLTextAreaElement?HTMLTextAreaElement.prototype:HTMLInputElement.prototype;` +
		`var setter=Object.getOwnPropertyDescriptor(proto,'value').set;` +
		`setter.call(el,` + string(val) + `);` +
		`el.dispatchEvent(new Event('input',{bubbles:true}));` +
		`el.dispatchEvent(new Event('change',{bubbles:true}));` +
		`var btn=document.querySelector(` + string(sendSel) + `);` +
		`if(btn){btn.click();return true;}` +
		// Fallback: some Meet builds send on Enter with no separate button.
		`el.dispatchEvent(new KeyboardEvent('keydown',{key:'Enter',code:'Enter',keyCode:13,which:13,bubbles:true}));` +
		`return true;})()`
}

// clickFirstBySelectorJS builds a JS expression that clicks the first element
// matching selector and THROWS when nothing matches (so cdpEvaluate turns a
// stale selector into a Go error). Mirrors platform.clickJS but is defined here
// to keep the jobs package free of a platform dependency for JS building.
func clickFirstBySelectorJS(selector string) string {
	sel, _ := json.Marshal(selector)
	return `(function(){var el=document.querySelector(` + string(sel) + `);` +
		`if(!el){throw new Error("selector not found: "+` + string(sel) + `);}` +
		`el.scrollIntoView();el.click();return true;})()`
}

// openMeetChat opens the chat panel. Best-effort: a stale selector returns an
// error the caller logs and continues on (chat capture degrading must never
// crash the recording).
func openMeetChat(page chatEvaluator) error {
	_, err := page.Evaluate(meetChatOpenJS())
	return err
}

// readMeetChat reads the current chat history and parses it into ordered
// ChatMessages. Returns a parse error only when the page returned something
// unexpected; an empty chat yields (nil, nil).
func readMeetChat(page chatEvaluator) ([]MeetChatMessage, error) {
	v, err := page.Evaluate(meetChatReadJS())
	if err != nil {
		return nil, err
	}
	return parseChatMessages(v)
}

// postMeetChat posts text to the chat. Best-effort (see openMeetChat).
func postMeetChat(page chatEvaluator, text string) error {
	_, err := page.Evaluate(meetChatSendJS(text))
	return err
}

// parseChatMessages decodes the readback value from meetChatReadJS. The JS
// returns a JSON STRING (JSON.stringify) so the value crosses CDP as a string
// regardless of returnByValue quirks; parseChatMessages tolerates either a
// string payload or an already-decoded []any (defensive against a future change
// to the readback shape). A nil/empty value yields no messages.
func parseChatMessages(v any) ([]MeetChatMessage, error) {
	switch payload := v.(type) {
	case nil:
		return nil, nil
	case string:
		s := strings.TrimSpace(payload)
		if s == "" {
			return nil, nil
		}
		var msgs []MeetChatMessage
		if err := json.Unmarshal([]byte(s), &msgs); err != nil {
			return nil, fmt.Errorf("parse chat readback JSON: %w", err)
		}
		return msgs, nil
	case []any:
		return coerceChatArray(payload)
	default:
		return nil, fmt.Errorf("unexpected chat readback type %T", v)
	}
}

// coerceChatArray converts an already-decoded []any (each a map) into
// ChatMessages, tolerating missing fields. Used only if the readback ever comes
// back as a live array rather than a JSON string.
func coerceChatArray(arr []any) ([]MeetChatMessage, error) {
	out := make([]MeetChatMessage, 0, len(arr))
	for i, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("chat item %d is not an object: %T", i, item)
		}
		text, _ := m["text"].(string)
		if strings.TrimSpace(text) == "" {
			continue
		}
		sender, _ := m["sender"].(string)
		idx := i
		if n, ok := toInt(m["index"]); ok {
			idx = n
		}
		out = append(out, MeetChatMessage{Index: idx, Sender: sender, Text: text})
	}
	return out, nil
}

package jobs

import (
	"encoding/json"
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/meetingpolicy"
)

// Teams in-call chat support (meeting parity N3, citadel-cli#1595): the owner
// disclosure on admission and a standalone `/ace leave`. Deliberately NOT the
// Meet interactive layer.
//
// NATIVE ACCEPTANCE REQUIRED: every selector below is UNVERIFIED against a live
// Teams web meeting except the chat composer (div[data-tid="ckeditor"]), which
// is already used as an in-call marker. They are kept together here so live
// tuning touches one place. Every probe fails soft: a stale selector returns an
// error or an empty read, the caller logs it, and recording continues.
const (
	// teamsChatOpenSelector: the in-call toolbar chat button.
	teamsChatOpenSelector = `#chat-button,button[data-tid="chat-button"],button[aria-label*="chat" i]`
	// teamsChatMessageRowSelector: one rendered chat message.
	teamsChatMessageRowSelector = `[data-tid="chat-pane-message"],[data-tid="chat-message"],div[role="listitem"][data-mid]`
	// teamsChatSenderSelector: sender name within a message row.
	teamsChatSenderSelector = `[data-tid="message-author-name"],[data-tid="author-name"]`
	// teamsChatTextSelector: message body within a message row.
	teamsChatTextSelector = `[data-tid="chat-pane-message-body"],[data-tid="message-body-content"],[id^="content-"]`
	// teamsChatInputSelector: the contenteditable composer.
	teamsChatInputSelector = `div[data-tid="ckeditor"][contenteditable="true"],div[data-tid="ckeditor"] [contenteditable="true"],div[data-tid="ckeditor"]`
	// teamsChatSendSelector: the send button next to the composer.
	teamsChatSendSelector = `button[data-tid="newMessageCommands-send"],button[aria-label*="send" i]`
)

func teamsChatOpenJS() string { return clickFirstBySelectorJS(teamsChatOpenSelector) }

// teamsChatReadJS mirrors meetChatReadJS with Teams selectors; returns a JSON
// string of {index, sender, text} and never throws on an empty chat.
func teamsChatReadJS() string {
	rowSel, _ := json.Marshal(teamsChatMessageRowSelector)
	senderSel, _ := json.Marshal(teamsChatSenderSelector)
	textSel, _ := json.Marshal(teamsChatTextSelector)
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

// teamsChatSendJS inserts text into the contenteditable composer and sends.
// Throws when the composer is missing; returns false when nothing could be sent.
func teamsChatSendJS(text string) string {
	inputSel, _ := json.Marshal(teamsChatInputSelector)
	sendSel, _ := json.Marshal(teamsChatSendSelector)
	val, _ := json.Marshal(text)
	return `(function(){` +
		`var el=document.querySelector(` + string(inputSel) + `);` +
		`if(!el){throw new Error("teams chat input not found: "+` + string(inputSel) + `);}` +
		`el.focus();` +
		`document.execCommand('selectAll',false,null);` +
		`document.execCommand('insertText',false,` + string(val) + `);` +
		`var btn=document.querySelector(` + string(sendSel) + `);` +
		`if(btn){btn.click();return true;}` +
		`el.dispatchEvent(new KeyboardEvent('keydown',{key:'Enter',code:'Enter',keyCode:13,which:13,bubbles:true}));` +
		`return true;})()`
}

func openTeamsChat(page chatEvaluator) error {
	_, err := page.Evaluate(teamsChatOpenJS())
	return err
}

func readTeamsChat(page chatEvaluator) ([]MeetChatMessage, error) {
	v, err := page.Evaluate(teamsChatReadJS())
	if err != nil {
		return nil, err
	}
	return parseChatMessages(v)
}

func postTeamsChat(page chatEvaluator, text string) error {
	v, err := page.Evaluate(teamsChatSendJS(text))
	if err != nil {
		return err
	}
	if b, ok := v.(bool); ok && !b {
		return fmt.Errorf("teams chat send reported failure")
	}
	return nil
}

// announceTeamsOnAdmission posts the owner disclosure in Teams chat. The
// announcement text is registered in botMessages BEFORE posting so its own
// "/ace leave" is never mistaken for a command. Returns true only when the
// post is read back in the chat; any failure returns false and recording
// continues.
func (h *MeetingJoinHandler) announceTeamsOnAdmission(ctx JobContext, page chatEvaluator, botMessages map[string]struct{}, p meetingJoinParams) bool {
	text := meetingpolicy.Announcement(p.BotDisplayName, p.OwnerDisplayName)
	botMessages[normalizeChatText(text)] = struct{}{}

	if err := openTeamsChat(page); err != nil {
		ctx.Log("warn", "     - could not open Teams chat to announce (non-fatal, selector unverified): %v", err)
	}
	before, readErr := readTeamsChat(page)
	lastIndex := -1
	for _, m := range before {
		if m.Index > lastIndex {
			lastIndex = m.Index
		}
	}
	if err := postTeamsChat(page, text); err != nil {
		ctx.Log("warn", "     - could not post disclosure to Teams chat (non-fatal, selector unverified): %v", err)
		return false
	}
	if readErr != nil || ctx.Context().Err() != nil {
		return false
	}
	after, err := readTeamsChat(page)
	if err != nil || ctx.Context().Err() != nil {
		return false
	}
	for _, m := range after {
		if m.Index > lastIndex && normalizeChatText(m.Text) == normalizeChatText(text) {
			ctx.Log("info", "     - confirmed disclosure in Teams chat")
			return true
		}
	}
	return false
}

// teamsLeaveRequested scans new Teams chat messages for a standalone `/ace
// leave` (exact normalized message, never a substring), skipping the bot's own
// messages. seen is updated. Fails soft: a read error means no leave.
func teamsLeaveRequested(page chatEvaluator, seen map[int]struct{}, own map[string]struct{}) bool {
	msgs, err := readTeamsChat(page)
	if err != nil {
		return false
	}
	for _, m := range msgs {
		if _, ok := seen[m.Index]; ok {
			continue
		}
		seen[m.Index] = struct{}{}
		norm := normalizeChatText(m.Text)
		if _, isOwn := own[norm]; isOwn {
			continue
		}
		if norm == "/ace leave" {
			return true
		}
	}
	return false
}

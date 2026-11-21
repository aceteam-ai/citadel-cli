package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/meetingpolicy"
)

// teamsChatPage is a fake Teams in-call page with a chat list.
type teamsChatPage struct {
	neverEndingPage
	msgs     []MeetChatMessage
	postErr  error
	noEcho   bool
	posted   []string
	left     bool
	readErr  error
	incoming []MeetChatMessage // appended on the first read after admission
}

func (p *teamsChatPage) Evaluate(js string) (any, error) {
	switch {
	case js == teamsChatOpenJS():
		return true, nil
	case js == teamsChatReadJS():
		if p.readErr != nil {
			return nil, p.readErr
		}
		b, _ := json.Marshal(p.msgs)
		return string(b), nil
	case js == teamsLeaveJS:
		p.left = true
		return true, nil
	case js == teamsIsEndedJS:
		return false, nil
	case strings.Contains(js, "teams chat input not found"):
		if p.postErr != nil {
			return nil, p.postErr
		}
		// recover the posted text from the embedded JSON literal
		i := strings.Index(js, "insertText',false,")
		rest := js[i+len("insertText',false,"):]
		end := strings.Index(rest, ");var btn")
		var txt string
		_ = json.Unmarshal([]byte(rest[:end]), &txt)
		p.posted = append(p.posted, txt)
		if !p.noEcho {
			p.msgs = append(p.msgs, MeetChatMessage{Index: len(p.msgs), Sender: "bot", Text: txt})
		}
		return true, nil
	}
	return p.neverEndingPage.Evaluate(js)
}

func teamsTestParams() meetingJoinParams {
	return meetingJoinParams{
		MeetingID: "m1", Platform: platformTeams,
		BotDisplayName: "Notetaker", OwnerDisplayName: "Pat",
		MaxDurationSeconds: 60,
	}
}

func TestTeamsDisclosurePostedOnAdmission(t *testing.T) {
	page := &teamsChatPage{}
	own := map[string]struct{}{}
	h := &MeetingJoinHandler{}
	ok := h.announceTeamsOnAdmission(JobContext{Ctx: context.Background()}, page, own, map[string]struct{}{}, teamsTestParams())
	if !ok || len(page.posted) != 1 {
		t.Fatalf("ok=%v posted=%v", ok, page.posted)
	}
	want := meetingpolicy.Announcement("Notetaker", "Pat")
	if page.posted[0] != want {
		t.Fatalf("text %q", page.posted[0])
	}
	if _, seeded := own[normalizeChatText(want)]; !seeded {
		t.Fatal("announcement not seeded as own message")
	}
}

func TestTeamsDisclosureFailedPostReportsFalse(t *testing.T) {
	h := &MeetingJoinHandler{}
	own := map[string]struct{}{}
	page := &teamsChatPage{postErr: errors.New("no composer")}
	if h.announceTeamsOnAdmission(JobContext{Ctx: context.Background()}, page, own, map[string]struct{}{}, teamsTestParams()) {
		t.Fatal("failed post must report false")
	}
	if len(own) != 1 {
		t.Fatal("echo guard must be seeded even on failure")
	}
	// post succeeds but never shows up in chat: not confirmed
	page = &teamsChatPage{noEcho: true}
	if h.announceTeamsOnAdmission(JobContext{Ctx: context.Background()}, page, map[string]struct{}{}, map[string]struct{}{}, teamsTestParams()) {
		t.Fatal("unconfirmed post must report false")
	}
}

func TestTeamsLeaveOnStandaloneCommandOnly(t *testing.T) {
	own := map[string]struct{}{normalizeChatText(meetingpolicy.Announcement("Notetaker", "Pat")): {}}
	page := &teamsChatPage{msgs: []MeetChatMessage{
		{Index: 0, Text: meetingpolicy.Announcement("Notetaker", "Pat")}, // own echo
		{Index: 1, Text: "please type /ace leave to remove it"},          // substring
	}}
	seen := map[string]struct{}{}
	if teamsLeaveRequested(page, seen, own) {
		t.Fatal("echo/substring must not leave")
	}
	page.msgs = append(page.msgs, MeetChatMessage{Index: 2, Sender: "Sam", Text: "  /ACE   leave "})
	if !teamsLeaveRequested(page, seen, own) {
		t.Fatal("standalone command must leave")
	}
	page.readErr = errors.New("stale")
	if teamsLeaveRequested(page, map[string]struct{}{}, own) {
		t.Fatal("read failure must fail soft")
	}
}

func TestTeamsWaitLeavesOnAceLeave(t *testing.T) {
	page := &teamsChatPage{msgs: []MeetChatMessage{{Index: 0, Text: "/ace leave"}}}
	h := &MeetingJoinHandler{}
	reason, err := h.waitForMeetingEnd(JobContext{Ctx: context.Background()}, page, teamsTestParams(), deathTestMedia{}, map[string]struct{}{})
	if err != nil || reason != "ace_command_leave" || !page.left {
		t.Fatalf("reason=%q err=%v left=%v", reason, err, page.left)
	}
}

func TestTeamsWaitCancellationStillPrompt(t *testing.T) {
	page := &teamsChatPage{}
	h := &MeetingJoinHandler{}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	start := time.Now()
	reason, err := h.waitForMeetingEnd(JobContext{Ctx: ctx}, page, teamsTestParams(), deathTestMedia{}, map[string]struct{}{})
	if reason != "cancelled" || err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("reason=%q err=%v after %s", reason, err, time.Since(start))
	}
}

func TestTeamsPreexistingLeaveIgnoredNewOneLeaves(t *testing.T) {
	page := &teamsChatPage{msgs: []MeetChatMessage{{Index: 0, Sender: "Sam", Text: "/ace leave"}}}
	h := &MeetingJoinHandler{}
	own, seen := map[string]struct{}{}, map[string]struct{}{}
	jc := JobContext{Ctx: context.Background()}
	if !h.announceTeamsOnAdmission(jc, page, own, seen, teamsTestParams()) {
		t.Fatal("disclosure should be confirmed")
	}
	if teamsLeaveRequested(page, seen, own) {
		t.Fatal("pre-existing /ace leave must not leave")
	}
	page.msgs = append(page.msgs, MeetChatMessage{Index: len(page.msgs), Sender: "Sam", Text: "/ace leave"})
	if !teamsLeaveRequested(page, seen, own) {
		t.Fatal("new /ace leave after admission must leave")
	}
}

func TestTeamsDisclosureReadbackRetries(t *testing.T) {
	old := teamsReadbackDelay
	teamsReadbackDelay = time.Millisecond
	defer func() { teamsReadbackDelay = old }()
	page := &teamsDelayedEchoPage{}
	h := &MeetingJoinHandler{}
	if !h.announceTeamsOnAdmission(JobContext{Ctx: context.Background()}, page, map[string]struct{}{}, map[string]struct{}{}, teamsTestParams()) {
		t.Fatal("echo appearing on the second read must be confirmed")
	}
	// never appears: bounded, reports false
	page = &teamsDelayedEchoPage{never: true}
	if h.announceTeamsOnAdmission(JobContext{Ctx: context.Background()}, page, map[string]struct{}{}, map[string]struct{}{}, teamsTestParams()) {
		t.Fatal("missing echo must report false")
	}
	if page.reads > 4 {
		t.Fatalf("too many reads: %d", page.reads)
	}
}

// teamsDelayedEchoPage renders the bot's post only from the second read after
// posting.
type teamsDelayedEchoPage struct {
	teamsChatPage
	never          bool
	pending        string
	readsSincePost int
	reads          int
}

func (p *teamsDelayedEchoPage) Evaluate(js string) (any, error) {
	if js == teamsChatReadJS() {
		p.reads++
		if p.pending != "" {
			p.readsSincePost++
			if p.readsSincePost >= 2 && !p.never {
				p.msgs = append(p.msgs, MeetChatMessage{Index: len(p.msgs), Text: p.pending})
				p.pending = ""
			}
		}
		return p.teamsChatPage.Evaluate(js)
	}
	if strings.Contains(js, "teams chat input not found") {
		p.noEcho = true
		_, err := p.teamsChatPage.Evaluate(js)
		p.pending = p.posted[len(p.posted)-1]
		return true, err
	}
	return p.teamsChatPage.Evaluate(js)
}

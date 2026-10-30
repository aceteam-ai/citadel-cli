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

type disclosureBrowser struct {
	text                                 string
	posts, reads                         int
	confirmed, preexisting, ended, leave bool
	postErr, readErr                     error
	guard                                map[string]struct{}
	onPost                               func()
	onEnd                                func()
	onRead                               func(int)
}

func (b *disclosureBrowser) Navigate(string) error { return nil }
func (b *disclosureBrowser) CurrentURL() (string, error) {
	return "https://meet.google.com/synthetic", nil
}
func (b *disclosureBrowser) Type(string, string) error { return nil }
func (b *disclosureBrowser) Close() error              { return nil }
func (b *disclosureBrowser) Evaluate(js string) (any, error) {
	switch {
	case js == meetLeaveCallJS:
		b.leave = true
		return true, nil
	case js == meetIsAdmittedJS:
		return true, nil
	case js == meetIsEndedJS:
		if b.onEnd != nil {
			b.onEnd()
		}
		return b.ended, nil
	case js == meetParticipantCountJS:
		return float64(2), nil
	case strings.Contains(js, "JSON.stringify"):
		b.reads++
		if b.onRead != nil {
			b.onRead(b.reads)
		}
		if b.readErr != nil {
			return nil, b.readErr
		}
		var messages []MeetChatMessage
		if b.preexisting || b.posts > 0 && b.confirmed {
			messages = append(messages, MeetChatMessage{Index: 1, Text: b.text})
		}
		if b.posts > 0 && b.reads >= 3 && !b.ended && b.onEnd == nil {
			messages = append(messages, MeetChatMessage{Index: 2, Text: "/ace leave"})
		}
		out, _ := json.Marshal(messages)
		return string(out), nil
	case strings.Contains(js, "chat input not found"):
		b.posts++
		if b.guard != nil {
			if _, ok := b.guard[normalizeChatText(b.text)]; !ok {
				return nil, errors.New("post ran before own-message fence")
			}
		}
		if b.onPost != nil {
			b.onPost()
		}
		return true, b.postErr
	default:
		return true, nil
	}
}
func TestMeetingDisclosureRequiresNewReadbackAndSeedsBeforePost(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		confirm, preexisting bool
		postErr, readErr     error
		want                 bool
	}{
		{"confirmed", true, false, nil, nil, true}, {"selector only", false, false, nil, nil, false}, {"old identical message", true, true, nil, nil, false},
		{"post failed", false, false, errors.New("synthetic post failure"), nil, false}, {"read failed", true, false, nil, errors.New("synthetic read failure"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testParams()
			p.BotDisplayName = "Recorder"
			p.OwnerDisplayName = "Jason"
			guard := map[string]struct{}{}
			b := &disclosureBrowser{text: meetingpolicy.Announcement("Recorder", "Jason"), confirmed: tc.confirm, preexisting: tc.preexisting, postErr: tc.postErr, readErr: tc.readErr, guard: guard}
			h := &MeetingJoinHandler{}
			if got := h.announceOnAdmission(JobContext{}, b, guard, p); got != tc.want {
				t.Fatalf("disclosure=%v want%v", got, tc.want)
			}
			if _, ok := guard[normalizeChatText(b.text)]; !ok {
				t.Fatal("dynamic echo fence missing")
			}
			if b.posts != 1 {
				t.Fatal("disclosure was not attempted exactly once")
			}
		})
	}
}
func TestMeetingDisclosureCancellationCannotBecomeConfirmed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := testParams()
	p.BotDisplayName = "Recorder"
	p.OwnerDisplayName = "Owner"
	b := &disclosureBrowser{text: meetingpolicy.Announcement("Recorder", "Owner"), confirmed: true, onPost: cancel}
	if (&MeetingJoinHandler{}).announceOnAdmission(JobContext{Ctx: ctx}, b, map[string]struct{}{}, p) {
		t.Fatal("cancelled post became confirmed")
	}
}
func TestMeetingDisclosureRunsWithoutStreamingAndDoesNotClaimTeamsChat(t *testing.T) {
	for _, stream := range []bool{false, true} {
		h := &MeetingJoinHandler{StreamingEnabled: stream, StreamingInterval: time.Hour}
		p := testParams()
		p.OwnerDisplayName = "Owner"
		p.BotDisplayName = "Recorder"
		b := &disclosureBrowser{text: meetingpolicy.Announcement("Recorder", "Owner"), confirmed: true, ended: true}
		out, err := h.runMeetingLoop(JobContext{}, b, p, "unused", deathTestMedia{})
		if err != nil || !out.disclosurePosted || b.posts != 1 {
			t.Fatalf("stream%v disclosure%v posts%d err%v", stream, out.disclosurePosted, b.posts, err)
		}
	}
	p := testParams()
	p.Platform = platformTeams
	p.recordingStartedAt = time.Now().Add(-2 * time.Second)
	p.MaxDurationSeconds = 1
	b := &disclosureBrowser{ended: true}
	out, err := (&MeetingJoinHandler{StreamingEnabled: true}).runMeetingLoop(JobContext{}, b, p, "unused", deathTestMedia{})
	if err != nil || out.disclosurePosted || b.posts != 0 {
		t.Fatal("Teams disclosure falsely claimed")
	}
}
func TestMeetingPlainModeHonorsStandaloneLeaveWithoutSelfEcho(t *testing.T) {
	p := testParams()
	p.BotDisplayName = "Recorder"
	p.OwnerDisplayName = "Owner /ace leave"
	b := &disclosureBrowser{text: meetingpolicy.Announcement(p.BotDisplayName, p.OwnerDisplayName), confirmed: true}
	out, err := (&MeetingJoinHandler{}).runMeetingLoop(JobContext{}, b, p, "unused", deathTestMedia{})
	if err != nil || out.endReason != "ace_command_leave" || !b.leave || b.reads < 3 {
		t.Fatal(out, err, b.reads)
	}
}
func TestMeetingPayloadNameFallbackAndConfiguredDuration(t *testing.T) {
	profile := meetingpolicy.Default()
	profile.RecordingDefault = 600
	p, err := parseMeetingJoinParamsWithProfile(map[string]string{"meeting_url": "https://meet.google.com/synthetic", "meeting_id": "synthetic", "bot_display_name": " Custom ", "ownerName": "must not alias", "owner_email": "fixture@example.invalid", "owner_id": "must-not-lookup"}, profile)
	if err != nil || p.BotDisplayName != "Custom" || p.OwnerDisplayName != meetingpolicy.OwnerFallback || p.maxDuration() != 10*time.Minute {
		t.Fatal(p, err)
	}
	if p.mediaLifetime() != 18*time.Minute {
		t.Fatal("session cap lost setup/admission/cleanup")
	}
}

func TestMeetingDisclosureRejectsCancellationDuringReadback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &disclosureBrowser{text: meetingpolicy.Announcement("", ""), confirmed: true,
		onRead: func(reads int) {
			if reads == 2 {
				cancel()
			}
		}}
	if (&MeetingJoinHandler{}).announceOnAdmission(JobContext{Ctx: ctx}, b, map[string]struct{}{}) {
		t.Fatal("readback after cancellation confirmed disclosure")
	}
}

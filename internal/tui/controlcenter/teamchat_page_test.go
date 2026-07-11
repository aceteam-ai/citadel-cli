package controlcenter

import (
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/teamchat"
)

func tsAt(t time.Time) teamchat.Timestamp {
	return teamchat.Timestamp{Time: t}
}

func TestFormatTeamChatMessage(t *testing.T) {
	name := "Jason Sun"
	parent := "m-1"
	created := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name     string
		msg      teamchat.Message
		contains []string
		excludes []string
	}{
		{
			name: "human message",
			msg: teamchat.Message{
				ID: "m-2", Content: "hello team", MessageType: "user",
				CreatedAt: tsAt(created),
				Sender:    &teamchat.Sender{ID: "u-1", Email: "j@x.c", FullName: &name},
			},
			contains: []string{"[cyan]Jason Sun[white]", "hello team"},
			excludes: []string{"replies", "↳"},
		},
		{
			name: "agent message",
			msg: teamchat.Message{
				ID: "m-3", Content: "on it", MessageType: "agent",
				CreatedAt: tsAt(created),
				Agent:     &teamchat.AgentSummary{ID: "a-1", Title: "Ace"},
			},
			contains: []string{"[magenta]Ace[white]", "on it"},
		},
		{
			name: "threaded reply with reply count",
			msg: teamchat.Message{
				ID: "m-4", Content: "reply", MessageType: "user",
				CreatedAt:       tsAt(created),
				ParentMessageID: &parent,
				ReplyCount:      3,
			},
			contains: []string{"↳", "(3 replies)"},
		},
		{
			name: "attachment-only message",
			msg: teamchat.Message{
				ID: "m-5", Content: "", MessageType: "user",
				CreatedAt:   tsAt(created),
				Attachments: []teamchat.Attachment{{ID: "att-1", FileName: "report.pdf"}},
			},
			contains: []string{"(1 attachment(s))", "+ attachment: report.pdf"},
		},
		{
			name: "markup in content is escaped",
			msg: teamchat.Message{
				ID: "m-6", Content: "danger [red]text[white]", MessageType: "user",
				CreatedAt: tsAt(created),
			},
			// tview.Escape turns [red] into [red[] so user content can't
			// inject color codes.
			contains: []string{"[red[]"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatTeamChatMessage(tc.msg)
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Errorf("formatTeamChatMessage = %q, missing %q", got, want)
				}
			}
			for _, not := range tc.excludes {
				if strings.Contains(got, not) {
					t.Errorf("formatTeamChatMessage = %q, should not contain %q", got, not)
				}
			}
		})
	}
}

func TestParseSearchCommand(t *testing.T) {
	cases := []struct {
		input     string
		wantQuery string
		wantOK    bool
	}{
		{"/search deploy plan", "deploy plan", true},
		{"/search   spaced   ", "spaced", true},
		{"/search", "", false},
		{"/search   ", "", false},
		{"hello /search foo", "", false},
		{"regular message", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			query, ok := parseSearchCommand(tc.input)
			if query != tc.wantQuery || ok != tc.wantOK {
				t.Errorf("parseSearchCommand(%q) = (%q, %v), want (%q, %v)",
					tc.input, query, ok, tc.wantQuery, tc.wantOK)
			}
		})
	}
}

func TestNewestMessageID(t *testing.T) {
	if got := newestMessageID(nil); got != "" {
		t.Errorf("newestMessageID(nil) = %q, want empty", got)
	}
	msgs := []teamchat.Message{{ID: "m-1"}, {ID: "m-2"}, {ID: "m-3"}}
	if got := newestMessageID(msgs); got != "m-3" {
		t.Errorf("newestMessageID = %q, want m-3 (messages are oldest-first)", got)
	}
}

func TestTeamChatPageInterface(t *testing.T) {
	// TeamChatPage must satisfy the Page interface and carry stable
	// name/title used by PageManager.Show("teamchat") in ShowChat.
	var page Page = NewTeamChatPage(TeamChatPageConfig{})
	if page.Name() != "teamchat" {
		t.Errorf("Name = %q, want teamchat", page.Name())
	}
	if page.Title() != "Team Chat" {
		t.Errorf("Title = %q, want Team Chat", page.Title())
	}
}

func TestNodeChatPageRetitled(t *testing.T) {
	// The legacy node-to-node pub/sub chat keeps its registered name "chat"
	// (ShowChat depends on it) but is retitled to distinguish it from the
	// Team Chat tab.
	page := NewChatPage(ChatPageConfig{})
	if page.Name() != "chat" {
		t.Errorf("Name = %q, want chat", page.Name())
	}
	if page.Title() != "Node Chat" {
		t.Errorf("Title = %q, want Node Chat", page.Title())
	}
}

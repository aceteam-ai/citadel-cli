package teamchat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newTestClient spins up an httptest server with the given handler and
// returns a Client pointed at it.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(ClientConfig{BaseURL: srv.URL, Token: "act_test123"})
}

func TestListChannels(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/channels" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer act_test123" {
			t.Errorf("Authorization = %q, want Bearer act_test123", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"channels":[
			{"id":"ch-1","name":"general","visibility":"organization","organization_id":"org-1","created_by":"u-1"},
			{"id":"ch-2","name":"eng","description":"engineering","visibility":"public","organization_id":"org-1","created_by":"u-1"}
		]}`))
	})

	channels, err := client.ListChannels(context.Background())
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	if len(channels) != 2 {
		t.Fatalf("got %d channels, want 2", len(channels))
	}
	if channels[0].Name != "general" || channels[1].Name != "eng" {
		t.Errorf("unexpected channel names: %q, %q", channels[0].Name, channels[1].Name)
	}
	if channels[1].Description == nil || *channels[1].Description != "engineering" {
		t.Errorf("channel 2 description not decoded: %v", channels[1].Description)
	}
}

func TestMessagesPagination(t *testing.T) {
	var gotQuery string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/channels/ch-1/messages" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"messages":[
				{"id":"m-1","channel_id":"ch-1","content":"hello","created_at":"2026-07-01T12:00:00+00:00","message_type":"user","reply_count":0,
				 "sender":{"id":"u-1","email":"jason@example.com","full_name":"Jason Sun"}},
				{"id":"m-2","channel_id":"ch-1","content":"hi from an agent","created_at":"2026-07-01T12:00:05.123456+00:00","message_type":"agent","reply_count":2,
				 "agent":{"id":"a-1","title":"Ace"},"sender":null}
			],
			"hasMore":true,"nextCursor":"m-1"
		}`))
	})

	page, err := client.Messages(context.Background(), "ch-1", MessagesOptions{Limit: 50, Cursor: "m-9", Direction: "before"})
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if gotQuery != "cursor=m-9&direction=before&limit=50" {
		t.Errorf("query = %q", gotQuery)
	}
	if len(page.Messages) != 2 || !page.HasMore || page.NextCursor == nil || *page.NextCursor != "m-1" {
		t.Fatalf("unexpected page: %+v", page)
	}

	human, agent := page.Messages[0], page.Messages[1]
	if human.SenderLabel() != "Jason Sun" {
		t.Errorf("human SenderLabel = %q, want Jason Sun", human.SenderLabel())
	}
	if agent.SenderLabel() != "Ace" {
		t.Errorf("agent SenderLabel = %q, want Ace", agent.SenderLabel())
	}
	want := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if !human.CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %v, want %v", human.CreatedAt.Time, want)
	}
}

func TestSendMessage(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/channels/ch-1/messages" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["content"] != "ship it" {
			t.Errorf("content = %v", body["content"])
		}
		if body["parent_message_id"] != "m-7" {
			t.Errorf("parent_message_id = %v", body["parent_message_id"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"message":{"id":"m-10","channel_id":"ch-1","content":"ship it","created_at":"2026-07-01T12:01:00+00:00","message_type":"user","reply_count":0}}`))
	})

	msg, err := client.SendMessage(context.Background(), "ch-1", "ship it", "m-7")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if msg.ID != "m-10" || msg.Content != "ship it" {
		t.Errorf("unexpected message: %+v", msg)
	}
}

func TestSendMessageOmitsEmptyParent(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if _, present := body["parent_message_id"]; present {
			t.Error("parent_message_id should be omitted when empty")
		}
		w.Write([]byte(`{"message":{"id":"m-11","channel_id":"ch-1","content":"x","message_type":"user","reply_count":0}}`))
	})

	if _, err := client.SendMessage(context.Background(), "ch-1", "x", ""); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
}

func TestListMembers(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/channels/ch-1/members" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Write([]byte(`{"members":[
			{"id":"cm-1","user_id":"u-1","role":"admin","source":"direct","user":{"id":"u-1","email":"jason@example.com","full_name":"Jason Sun"}},
			{"id":"team:t-1:u-2","user_id":"u-2","role":"member","source":"team","user":{"id":"u-2","email":"justin@example.com"}}
		]}`))
	})

	members, err := client.ListMembers(context.Background(), "ch-1")
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("got %d members, want 2", len(members))
	}
	if members[0].Label() != "Jason Sun" {
		t.Errorf("member 0 label = %q", members[0].Label())
	}
	if members[1].Label() != "justin@example.com" {
		t.Errorf("member 1 label = %q", members[1].Label())
	}
}

func TestSearchMessages(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/channels/ch-1/messages/search" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("q"); got != "deploy plan" {
			t.Errorf("q = %q", got)
		}
		if got := r.URL.Query().Get("limit"); got != "25" {
			t.Errorf("limit = %q", got)
		}
		w.Write([]byte(`{"messages":[{"id":"m-3","channel_id":"ch-1","content":"the deploy plan is ready","message_type":"user","reply_count":0}]}`))
	})

	results, err := client.SearchMessages(context.Background(), "ch-1", "deploy plan", 25)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(results) != 1 || results[0].ID != "m-3" {
		t.Fatalf("unexpected results: %+v", results)
	}
}

func TestMarkRead(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/channels/ch-1/mark-read" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["messageId"] != "m-10" {
			t.Errorf("messageId = %v", body["messageId"])
		}
		w.Write([]byte(`{"success":true}`))
	})

	if err := client.MarkRead(context.Background(), "ch-1", "m-10"); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
}

func TestUnreadCounts(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/channels/unread-counts" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Write([]byte(`{"counts":{"ch-1":3,"ch-2":0}}`))
	})

	counts, err := client.UnreadCounts(context.Background())
	if err != nil {
		t.Fatalf("UnreadCounts: %v", err)
	}
	if counts["ch-1"] != 3 || counts["ch-2"] != 0 {
		t.Errorf("unexpected counts: %v", counts)
	}
}

func TestScopeDeniedIsAuthError(t *testing.T) {
	// A device API token hitting /api/channels/** gets 403 SCOPE_DENIED from
	// the platform's endpoint whitelist. The client must surface this as an
	// auth error so the TUI can render setup guidance instead of a raw 403.
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"Endpoint '/api/channels' is not in allowed endpoints list"}`))
	})

	_, err := client.ListChannels(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsAuthError(err) {
		t.Errorf("IsAuthError = false for 403, err = %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not *APIError: %v", err)
	}
	if apiErr.Message != "Endpoint '/api/channels' is not in allowed endpoints list" {
		t.Errorf("message = %q", apiErr.Message)
	}
}

func TestUnauthorizedIsAuthError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"Invalid API key"}`))
	})

	_, err := client.ListChannels(context.Background())
	if !IsAuthError(err) {
		t.Errorf("IsAuthError = false for 401, err = %v", err)
	}
}

func TestServerErrorIsNotAuthError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`oops`))
	})

	_, err := client.ListChannels(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if IsAuthError(err) {
		t.Error("IsAuthError = true for 500")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("unexpected error: %v", err)
	}
	if apiErr.Message != "oops" {
		t.Errorf("message = %q, want body snippet", apiErr.Message)
	}
}

func TestTimestampFormats(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  time.Time
	}{
		{"rfc3339 offset", `"2026-07-01T12:00:00+00:00"`, time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)},
		{"rfc3339 z", `"2026-07-01T12:00:00Z"`, time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)},
		{"fractional", `"2026-07-01T12:00:00.123456+00:00"`, time.Date(2026, 7, 1, 12, 0, 0, 123456000, time.UTC)},
		{"no offset", `"2026-07-01T12:00:00"`, time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)},
		{"empty", `""`, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ts Timestamp
			if err := json.Unmarshal([]byte(tc.input), &ts); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.input, err)
			}
			if !ts.Equal(tc.want) {
				t.Errorf("got %v, want %v", ts.Time, tc.want)
			}
		})
	}

	var ts Timestamp
	if err := json.Unmarshal([]byte(`"not-a-time"`), &ts); err == nil {
		t.Error("expected error for garbage timestamp")
	}
}

func TestSenderLabelFallbacks(t *testing.T) {
	empty := ""
	cases := []struct {
		name string
		msg  Message
		want string
	}{
		{"agent no join", Message{MessageType: "agent"}, "agent"},
		{"user email only", Message{MessageType: "user", Sender: &Sender{Email: "a@b.c"}}, "a@b.c"},
		{"user blank name", Message{MessageType: "user", Sender: &Sender{Email: "a@b.c", FullName: &empty}}, "a@b.c"},
		{"no sender", Message{MessageType: "user"}, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.msg.SenderLabel(); got != tc.want {
				t.Errorf("SenderLabel = %q, want %q", got, tc.want)
			}
		})
	}
}

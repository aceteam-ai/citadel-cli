package jobs

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

func TestParseMeetingJoinParams_Valid(t *testing.T) {
	p, err := parseMeetingJoinParams(map[string]string{
		"meeting_url":          "https://meet.google.com/abc-defg-hij",
		"meeting_id":           "mtg-123",
		"bot_display_name":     "Custom Bot",
		"max_duration_seconds": "600",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.MeetingURL != "https://meet.google.com/abc-defg-hij" {
		t.Errorf("MeetingURL = %q", p.MeetingURL)
	}
	if p.MeetingID != "mtg-123" {
		t.Errorf("MeetingID = %q", p.MeetingID)
	}
	if p.BotDisplayName != "Custom Bot" {
		t.Errorf("BotDisplayName = %q", p.BotDisplayName)
	}
	if p.MaxDurationSeconds != 600 {
		t.Errorf("MaxDurationSeconds = %d", p.MaxDurationSeconds)
	}
}

func TestParseMeetingJoinParams_DefaultsBotName(t *testing.T) {
	p, err := parseMeetingJoinParams(map[string]string{
		"meeting_url": "https://meet.google.com/x",
		"meeting_id":  "id1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.BotDisplayName != defaultBotDisplayName {
		t.Errorf("expected default bot name %q, got %q", defaultBotDisplayName, p.BotDisplayName)
	}
	if p.MaxDurationSeconds != 0 {
		t.Errorf("expected unset max duration, got %d", p.MaxDurationSeconds)
	}
	// Unset max duration resolves to the safety cap.
	if p.maxDuration() != defaultMeetingMaxDuration {
		t.Errorf("maxDuration() = %v, want default %v", p.maxDuration(), defaultMeetingMaxDuration)
	}
}

func TestParseMeetingJoinParams_MissingRequired(t *testing.T) {
	cases := map[string]map[string]string{
		"missing url": {"meeting_id": "id1"},
		"missing id":  {"meeting_url": "https://meet.google.com/x"},
		"blank url":   {"meeting_url": "   ", "meeting_id": "id1"},
		"empty":       {},
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseMeetingJoinParams(payload); err == nil {
				t.Errorf("expected error for %s, got nil", name)
			}
		})
	}
}

func TestParseMeetingJoinParams_MaxDuration(t *testing.T) {
	// Float string (JSON number coerced via fmt.Sprint) is accepted and truncated.
	p, err := parseMeetingJoinParams(map[string]string{
		"meeting_url":          "https://meet.google.com/x",
		"meeting_id":           "id1",
		"max_duration_seconds": "300.0",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.MaxDurationSeconds != 300 {
		t.Errorf("MaxDurationSeconds = %d, want 300", p.MaxDurationSeconds)
	}

	// Non-positive and non-numeric are rejected.
	for _, bad := range []string{"0", "-5", "abc"} {
		if _, err := parseMeetingJoinParams(map[string]string{
			"meeting_url":          "https://meet.google.com/x",
			"meeting_id":           "id1",
			"max_duration_seconds": bad,
		}); err == nil {
			t.Errorf("expected error for max_duration_seconds=%q, got nil", bad)
		}
	}
}

func TestSanitizeMeetingFilename(t *testing.T) {
	cases := map[string]string{
		"mtg-123":                 "mtg-123",
		"abc_DEF":                 "abc_DEF",
		"../../etc/passwd":        "______etc_passwd",
		"a/b\\c":                  "a_b_c",
		"":                        "meeting",
		"550e8400-e29b-41d4":      "550e8400-e29b-41d4",
	}
	for in, want := range cases {
		if got := sanitizeMeetingFilename(in); got != want {
			t.Errorf("sanitizeMeetingFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMeetingWavPath(t *testing.T) {
	got := meetingWavPath("/ws", "mtg-1")
	want := filepath.Join("/ws", "meetings", "mtg-1.wav")
	if got != want {
		t.Errorf("meetingWavPath = %q, want %q", got, want)
	}
	// A traversal-y meeting id cannot escape the meetings dir.
	got = meetingWavPath("/ws", "../../evil")
	if !strings.HasPrefix(got, filepath.Join("/ws", "meetings")) {
		t.Errorf("meetingWavPath escaped meetings dir: %q", got)
	}
}

func TestParsePositiveSeconds(t *testing.T) {
	ok := map[string]int{"1": 1, "300": 300, "300.0": 300, "59.9": 59}
	for in, want := range ok {
		got, err := parsePositiveSeconds(in)
		if err != nil {
			t.Errorf("parsePositiveSeconds(%q) errored: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parsePositiveSeconds(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"0", "-1", "-2.5", "", "nan-ish", "abc"} {
		if _, err := parsePositiveSeconds(bad); err == nil {
			t.Errorf("expected error for %q, got nil", bad)
		}
	}
}

func TestClickButtonByTextJS(t *testing.T) {
	labels := []string{"Ask to join", "Join now"}
	js := clickButtonByTextJS(labels)
	arr, _ := json.Marshal(labels)
	if !strings.Contains(js, string(arr)) {
		t.Errorf("clickButtonByTextJS did not embed labels array; got: %s", js)
	}
	if !strings.Contains(js, "throw new Error") {
		t.Errorf("clickButtonByTextJS must throw when no button matches; got: %s", js)
	}
	// Optional variant returns "" instead of throwing.
	opt := clickButtonByTextOptionalJS(labels)
	if strings.Contains(opt, "throw new Error") {
		t.Errorf("optional variant must not throw; got: %s", opt)
	}
	if !strings.Contains(opt, `return "";`) {
		t.Errorf("optional variant must return empty string on no match; got: %s", opt)
	}
}

func TestToInt(t *testing.T) {
	cases := []struct {
		in   any
		want int
		ok   bool
	}{
		{float64(3), 3, true},
		{int(5), 5, true},
		{int64(7), 7, true},
		{"nope", 0, false},
		{nil, 0, false},
	}
	for _, c := range cases {
		got, ok := toInt(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("toInt(%v) = (%d,%v), want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestMeetingJoin_MissingWorkspace(t *testing.T) {
	h := NewMeetingJoinHandler("")
	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "m1",
		Type: JobTypeTranscribeAudioType,
		Payload: map[string]string{
			"meeting_url": "https://meet.google.com/x",
			"meeting_id":  "id1",
		},
	})
	if err == nil {
		t.Fatal("expected error when workspace is unconfigured")
	}
}

package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		"mtg-123":            "mtg-123",
		"abc_DEF":            "abc_DEF",
		"../../etc/passwd":   "______etc_passwd",
		"a/b\\c":             "a_b_c",
		"":                   "meeting",
		"550e8400-e29b-41d4": "550e8400-e29b-41d4",
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

func TestClickButtonByTextOptionalJS(t *testing.T) {
	// Both label sets used by the poll loop must be embedded correctly.
	for name, labels := range map[string][]string{
		"join":    meetJoinButtonLabels,
		"dismiss": meetDismissButtonLabels,
	} {
		t.Run(name, func(t *testing.T) {
			js := clickButtonByTextOptionalJS(labels)
			arr, _ := json.Marshal(labels)
			if !strings.Contains(js, string(arr)) {
				t.Errorf("clickButtonByTextOptionalJS did not embed labels array; got: %s", js)
			}
			// The optional variant returns "" instead of throwing, so the poll
			// loop can retry a not-yet-rendered button without an error.
			if strings.Contains(js, "throw new Error") {
				t.Errorf("optional variant must not throw; got: %s", js)
			}
			if !strings.Contains(js, `return "";`) {
				t.Errorf("optional variant must return empty string on no match; got: %s", js)
			}
		})
	}
}

func TestJoinButtonTimeout_Sane(t *testing.T) {
	// The interstitial renders ~9s after navigation (observed live 2026-07-11)
	// and the pre-join page a few seconds after dismissal, so the poll window
	// must comfortably exceed that; it must also stay well under admitTimeout
	// so a stale-label failure surfaces before a lobby-length wait.
	if joinButtonTimeout < 20*time.Second {
		t.Errorf("joinButtonTimeout = %v, too short to outlast the ~9s interstitial + pre-join render", joinButtonTimeout)
	}
	if joinButtonTimeout >= admitTimeout {
		t.Errorf("joinButtonTimeout = %v must be shorter than admitTimeout %v", joinButtonTimeout, admitTimeout)
	}
	if meetingPollInterval >= joinButtonTimeout {
		t.Errorf("meetingPollInterval %v must be shorter than joinButtonTimeout %v", meetingPollInterval, joinButtonTimeout)
	}
}

// fakeJoinPage simulates the Meet pre-join DOM for pollForJoinClick: Evaluate
// answers each clickButtonByTextOptionalJS probe from a scripted queue keyed by
// which label set the JS embeds.
type fakeJoinPage struct {
	// joinResults are returned (then consumed) for successive join-label
	// probes; when exhausted, "" (no match) is returned.
	joinResults []any
	// dismissResults likewise for dismiss-label probes.
	dismissResults []any
	typeErr        error

	joinProbes    int
	dismissProbes int
	typedNames    []string
}

func (f *fakeJoinPage) Evaluate(expression string) (any, error) {
	joinArr, _ := json.Marshal(meetJoinButtonLabels)
	dismissArr, _ := json.Marshal(meetDismissButtonLabels)
	switch {
	case strings.Contains(expression, string(joinArr)):
		f.joinProbes++
		if len(f.joinResults) > 0 {
			v := f.joinResults[0]
			f.joinResults = f.joinResults[1:]
			if err, ok := v.(error); ok {
				return nil, err
			}
			return v, nil
		}
		return "", nil
	case strings.Contains(expression, string(dismissArr)):
		f.dismissProbes++
		if len(f.dismissResults) > 0 {
			v := f.dismissResults[0]
			f.dismissResults = f.dismissResults[1:]
			if err, ok := v.(error); ok {
				return nil, err
			}
			return v, nil
		}
		return "", nil
	}
	return nil, fmt.Errorf("fakeJoinPage: unexpected expression: %s", expression)
}

func (f *fakeJoinPage) Type(selector, text string) error {
	f.typedNames = append(f.typedNames, text)
	return f.typeErr
}

func TestPollForJoinClick_FirstPassSuccess(t *testing.T) {
	page := &fakeJoinPage{joinResults: []any{"ask to join"}}
	if err := pollForJoinClick(JobContext{}, page, "Bot", 200*time.Millisecond, time.Millisecond); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if page.joinProbes != 1 {
		t.Errorf("joinProbes = %d, want 1", page.joinProbes)
	}
	if page.dismissProbes != 1 {
		t.Errorf("dismissProbes = %d, want 1 (dismiss must run before the join probe)", page.dismissProbes)
	}
	if len(page.typedNames) != 1 || page.typedNames[0] != "Bot" {
		t.Errorf("typedNames = %v, want [Bot]", page.typedNames)
	}
}

func TestPollForJoinClick_InterstitialThenJoin(t *testing.T) {
	// Pass 1: interstitial dismissed, join button not rendered yet.
	// Pass 2: join button appears. Non-fatal dismiss misses and a Type error
	// must not abort the loop.
	page := &fakeJoinPage{
		dismissResults: []any{"continue without microphone", ""},
		joinResults:    []any{"", "join now"},
		typeErr:        fmt.Errorf("no name field (signed in)"),
	}
	if err := pollForJoinClick(JobContext{}, page, "Bot", 200*time.Millisecond, time.Millisecond); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if page.joinProbes != 2 {
		t.Errorf("joinProbes = %d, want 2", page.joinProbes)
	}
}

func TestPollForJoinClick_EvaluateErrorRetries(t *testing.T) {
	// A JS exception on one pass (page mid-render) is retried, not fatal.
	page := &fakeJoinPage{
		joinResults: []any{fmt.Errorf("javascript exception"), "join now"},
	}
	if err := pollForJoinClick(JobContext{}, page, "Bot", 200*time.Millisecond, time.Millisecond); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if page.joinProbes != 2 {
		t.Errorf("joinProbes = %d, want 2", page.joinProbes)
	}
}

func TestPollForJoinClick_TimeoutError(t *testing.T) {
	page := &fakeJoinPage{} // join button never appears
	err := pollForJoinClick(JobContext{}, page, "Bot", 200*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	// The backend (routes/meeting_bot.py) greps error strings; the prefix must
	// be preserved.
	if !strings.HasPrefix(err.Error(), "click join button:") {
		t.Errorf("error must keep the 'click join button:' prefix for backend error mapping; got: %v", err)
	}
	if page.joinProbes < 2 {
		t.Errorf("joinProbes = %d, want multiple passes before timing out", page.joinProbes)
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

// TestErrMeetingBotSignedOut_ErrorsIsDetectable exercises the whole point of
// exporting a sentinel (issue #5122): a caller several layers up (the worker
// adapter, an alerting hook) must be able to errors.Is-detect "the bot needs
// re-seeding" specifically, distinct from a generic join-flow failure like a
// stale selector or an admission timeout.
func TestErrMeetingBotSignedOut_ErrorsIsDetectable(t *testing.T) {
	wrapped := fmt.Errorf("%w: redirected to %s — re-seed docs/meeting-bot-profile-seeding.md",
		ErrMeetingBotSignedOut, "https://accounts.google.com/signin")
	if !errors.Is(wrapped, ErrMeetingBotSignedOut) {
		t.Fatalf("errors.Is(wrapped, ErrMeetingBotSignedOut) = false, want true; wrapped=%v", wrapped)
	}
	genericErr := fmt.Errorf("click join button: selector not found")
	if errors.Is(genericErr, ErrMeetingBotSignedOut) {
		t.Fatalf("a generic join-flow error must NOT match ErrMeetingBotSignedOut")
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

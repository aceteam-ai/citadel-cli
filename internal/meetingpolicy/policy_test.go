package meetingpolicy

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestRatifiedMeetingBudgets(t *testing.T) {
	for _, tc := range []struct {
		a, r    string
		ap, rp  bool
		e, wait int64
	}{
		{"", "", false, false, 29280, 29970}, {"600", "", true, false, 29700, 30390}, {"600", "21600", true, true, 36900, 37590}, {"1", "60", true, true, 14761, 15451},
	} {
		b, err := Default().Budget(tc.a, tc.ap, tc.r, tc.rp)
		if err != nil {
			t.Fatal(err)
		}
		if b.Execution() != time.Duration(tc.e)*time.Second || b.ProducerWait() != time.Duration(tc.wait)*time.Second || b.StuckAfter() != time.Duration(tc.e+300)*time.Second {
			t.Fatalf("wrong ratified budget: execution=%s wait=%s", b.Execution(), b.ProducerWait())
		}
		if b.Profile().Transcription != 14400 || b.Profile().Cleanup != 180 || b.Profile().Queue != 300 || b.Profile().Handoff != 30 || b.Profile().Setup != 120 || b.Profile().Receipt != 60 {
			t.Fatal("ratified phase changed")
		}
	}
}
func TestMeetingPayloadRequiresBoundedCanonicalNumbers(t *testing.T) {
	for _, raw := range []string{"", " ", " 600 ", "0", "00", "01", "+1", "-1", "1.1", "600.0", "1e2", "NaN", "Inf", "9223372036854775808", "private-synthetic-value"} {
		for _, field := range []string{"admission", "recording"} {
			var err error
			if field == "admission" {
				_, err = Default().Budget(raw, true, "", false)
			} else {
				_, err = Default().Budget("", false, raw, true)
			}
			if err == nil {
				t.Fatalf("accepted %s %q", field, raw)
			}
			if len(raw) > 3 && strings.Contains(err.Error(), raw) {
				t.Fatal("input reflected in error")
			}
		}
	}
	for _, a := range []string{"601", "21600"} {
		if _, err := Default().Budget(a, true, "", false); err == nil {
			t.Fatal("admission max removed")
		}
	}
	for _, r := range []string{"1", "59", "21601"} {
		if _, err := Default().Budget("", false, r, true); err == nil {
			t.Fatal("recording range removed")
		}
	}
}
func TestMeetingPolicyDefaultsAndOverrides(t *testing.T) {
	p, err := Config(nil).Resolve()
	if err != nil || p != Default() {
		t.Fatal("missing policy must use ratified defaults")
	}
	p, err = Config{"recording_max_seconds": "3600", "recording_default_seconds": "1800", "admission_max_seconds": "300"}.Resolve()
	if err != nil || p.RecordingMax != 3600 || p.RecordingDefault != 1800 || p.AdmissionMax != 300 {
		t.Fatal(p, err)
	}
	for _, c := range []Config{{"setup_seconds": "121"}, {"admission_max_seconds": "601"}, {"transcription_seconds": "14401"}, {"cleanup_seconds": "181"}, {"queue_seconds": "301"}, {"handoff_seconds": "31"}, {"recovery_seconds": "301"}, {"receipt_seconds": "61"}, {"recording_max_seconds": "21601"}, {"recording_default_seconds": "59"}, {"unknown": "1"}, {"setup_seconds": "0"}, {"setup_seconds": "1.5"}, {"admission_max_seconds": "100"}, {"recording_max_seconds": "3600"}} {
		if _, e := c.Resolve(); e == nil {
			t.Fatalf("accepted invalid policy: %v", c)
		}
	}
}
func TestMeetingExplicitAuthorityCannotBeExtended(t *testing.T) {
	b, _ := Default().Budget("600", true, "21600", true)
	if e := b.CheckAuthority("36900000", true, "36900", "37200"); e != nil {
		t.Fatal(e)
	}
	if e := b.CheckAuthority(nil, false, "", ""); e != nil {
		t.Fatal(e)
	}
	if e := b.CheckAuthority(nil, false, "0", ""); e != nil {
		t.Fatal(e)
	}
	for _, v := range []any{nil, true, -1, 0, "0", "36899999", "36900000.0", 36900000.5, math.NaN(), math.Inf(1), json.Number("3.69e7"), "9223372036854775807"} {
		if e := b.CheckAuthority(v, true, "", ""); e == nil {
			t.Fatalf("accepted invalid/short budget %v", v)
		}
	}
	for _, pair := range [][2]string{{"36899", ""}, {"", "37199"}, {"", "0"}, {"bad", ""}, {"", "-1"}, {"9223372036854775807", ""}} {
		if e := b.CheckAuthority(nil, false, pair[0], pair[1]); e == nil {
			t.Fatal("operator authority extended")
		}
	}
	var zero Budget
	if e := zero.CheckAuthority(nil, false, "", ""); e == nil {
		t.Fatal("zero budget authorized")
	}
}
func TestMeetingNamesArePresentationAndBounded(t *testing.T) {
	if OwnerFallback != "the meeting owner" {
		t.Fatal("ratified generic owner fallback changed")
	}
	for _, raw := range []string{"", "  ", "a\nb", "a\tb", "a\x00b", "a\u202eb", "a\u2028b", "\u200b", string([]byte{0xff}), strings.Repeat("界", 129)} {
		if got := Name(raw, OwnerFallback); got != OwnerFallback {
			t.Fatalf("unusable name did not fall back: %q", got)
		}
	}
	if got := Name("  Jason   Sun  ", OwnerFallback); got != "Jason Sun" {
		t.Fatal(got)
	}
	s := strings.Repeat("界", 128)
	if Name(s, OwnerFallback) != s {
		t.Fatal("Unicode boundary rejected")
	}
	text := Announcement("Recorder", "Jason")
	want := "Hi, I'm Recorder. I'm recording audio and transcribing this meeting for Jason. Anyone can type /ace leave to remove me, or the host can remove me."
	if text != want || strings.Contains(text, "video") {
		t.Fatal("wrong disclosure")
	}
	if !strings.Contains(Announcement("", ""), "AceTeam Notetaker") || !strings.Contains(Announcement("", ""), OwnerFallback) {
		t.Fatal("wrong legacy producer fallback")
	}
}

func TestNameAllowsJoinersButRejectsBidiControls(t *testing.T) {
	for _, ok := range []string{"می‌خواهم", "👩‍💻 Dev"} {
		if got := Name(ok, OwnerFallback); got != ok {
			t.Fatalf("%q fell back to %q", ok, got)
		}
	}
	for _, bad := range []string{"Bob‮evil", "Bob⁦x⁩", "a​b­"} {
		if strings.ContainsRune(bad, '​') {
			continue // ZWSP/SHY policy is unchanged; only joiners were widened
		}
		if got := Name(bad, OwnerFallback); got != OwnerFallback {
			t.Fatalf("%q was accepted as %q", bad, got)
		}
	}
}

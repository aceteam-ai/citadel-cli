// Package meetingpolicy owns #1505's finite meeting-specific defaults. It
// performs no I/O; queue and handoff allowances require paired source authority.
package meetingpolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	OwnerFallback             = "the meeting owner"
	BotFallback               = "AceTeam Notetaker"
	MinRecordingSeconds int64 = 60
)

// Config uses canonical decimal seconds, preserving YAML scalar spelling so
// fractional/overflowing configuration cannot be silently narrowed to integers.
// Missing entries use ratified defaults; present invalid entries fail closed.
type Config map[string]string

type Profile struct {
	Setup, AdmissionDefault, AdmissionMax         int64
	RecordingDefault, RecordingMax, Transcription int64
	Cleanup, Queue, Handoff, Recovery, Receipt    int64
}

func Default() Profile {
	return Profile{Setup: 120, AdmissionDefault: 180, AdmissionMax: 600,
		RecordingDefault: 14400, RecordingMax: 21600, Transcription: 14400,
		Cleanup: 180, Queue: 300, Handoff: 30, Recovery: 300, Receipt: 60}
}

var errPolicy = errors.New("invalid meeting timing policy")
var errPayload = errors.New("invalid meeting timing payload: bounded canonical whole seconds required")
var errCeiling = errors.New("meeting profile exceeds explicit job or operator timing authority")

func Decimal(raw string) (int64, bool) {
	if raw == "" || len(raw) > 19 || raw[0] < '1' || raw[0] > '9' {
		return 0, false
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	return n, err == nil && n > 0
}

func (c Config) Resolve() (Profile, error) {
	p := Default()
	fields := map[string]*int64{"setup_seconds": &p.Setup, "admission_default_seconds": &p.AdmissionDefault,
		"admission_max_seconds": &p.AdmissionMax, "recording_default_seconds": &p.RecordingDefault,
		"recording_max_seconds": &p.RecordingMax, "transcription_seconds": &p.Transcription,
		"cleanup_seconds": &p.Cleanup, "queue_seconds": &p.Queue, "handoff_seconds": &p.Handoff,
		"recovery_seconds": &p.Recovery, "receipt_seconds": &p.Receipt}
	for key, raw := range c {
		dst, ok := fields[key]
		if !ok {
			return Profile{}, errPolicy
		}
		n, ok := Decimal(raw)
		if !ok {
			return Profile{}, errPolicy
		}
		*dst = n
	}
	if err := p.Validate(); err != nil {
		return Profile{}, err
	}
	return p, nil
}

func (p Profile) Validate() error {
	d := Default()
	pairs := [][2]int64{{p.Setup, d.Setup}, {p.AdmissionDefault, p.AdmissionMax}, {p.AdmissionMax, d.AdmissionMax},
		{p.RecordingDefault, p.RecordingMax}, {p.RecordingMax, d.RecordingMax}, {p.Transcription, d.Transcription},
		{p.Cleanup, d.Cleanup}, {p.Queue, d.Queue}, {p.Handoff, d.Handoff}, {p.Recovery, d.Recovery}, {p.Receipt, d.Receipt}}
	for _, pair := range pairs {
		if pair[0] <= 0 || pair[0] > pair[1] {
			return errPolicy
		}
	}
	if p.RecordingDefault < MinRecordingSeconds || p.RecordingMax < MinRecordingSeconds {
		return errPolicy
	}
	return nil
}

type Budget struct {
	profile              Profile
	admission, recording int64
}

func (p Profile) Budget(admission string, admissionPresent bool, recording string, recordingPresent bool) (Budget, error) {
	if err := p.Validate(); err != nil {
		return Budget{}, err
	}
	a, r := p.AdmissionDefault, p.RecordingDefault
	if admissionPresent {
		var ok bool
		a, ok = Decimal(admission)
		if !ok || a > p.AdmissionMax {
			return Budget{}, errPayload
		}
	}
	if recordingPresent {
		var ok bool
		r, ok = Decimal(recording)
		if !ok || r < MinRecordingSeconds || r > p.RecordingMax {
			return Budget{}, errPayload
		}
	}
	return Budget{profile: p, admission: a, recording: r}, nil
}

func (p Profile) Maximum() (Budget, error) {
	return p.Budget(strconv.FormatInt(p.AdmissionMax, 10), true, strconv.FormatInt(p.RecordingMax, 10), true)
}

// Arithmetic follows validated bounds (at most 36,900 execution seconds).
func (b Budget) Profile() Profile         { return b.profile }
func (b Budget) Admission() time.Duration { return time.Duration(b.admission) * time.Second }
func (b Budget) Recording() time.Duration { return time.Duration(b.recording) * time.Second }
func (b Budget) Execution() time.Duration {
	return time.Duration(b.profile.Setup+b.admission+b.recording+b.profile.Transcription+b.profile.Cleanup) * time.Second
}
func (b Budget) StuckAfter() time.Duration {
	return b.Execution() + time.Duration(b.profile.Recovery)*time.Second
}
func (b Budget) ProducerWait() time.Duration {
	return b.StuckAfter() + time.Duration(b.profile.Queue+b.profile.Handoff+b.profile.Receipt)*time.Second
}

// CheckAuthority never extends a finite explicit ceiling. A zero generic long
// timeout stays unbounded, but meetings require a finite policy and STUCK recovery.
func (b Budget) CheckAuthority(timeout any, present bool, longRaw, stuckRaw string) error {
	if b.profile.Validate() != nil || b.admission <= 0 || b.admission > b.profile.AdmissionMax || b.recording < MinRecordingSeconds || b.recording > b.profile.RecordingMax {
		return errPolicy
	}
	if present {
		ms, ok := Milliseconds(timeout)
		if !ok || time.Duration(ms)*time.Millisecond < b.Execution() {
			return errCeiling
		}
	}
	for _, ceiling := range []struct {
		raw  string
		need time.Duration
	}{{longRaw, b.Execution()}, {stuckRaw, b.StuckAfter()}} {
		if ceiling.raw == "" || ceiling.raw == "0" {
			continue
		}
		n, ok := Decimal(ceiling.raw)
		if !ok || n > int64(math.MaxInt64/time.Second) || time.Duration(n)*time.Second < ceiling.need {
			return errCeiling
		}
	}
	if stuckRaw == "0" {
		return errCeiling
	}
	return nil
}

func Milliseconds(value any) (int64, bool) {
	var raw string
	switch v := value.(type) {
	case string:
		raw = v
	case json.Number:
		raw = string(v)
	case int:
		raw = strconv.FormatInt(int64(v), 10)
	case int64:
		raw = strconv.FormatInt(v, 10)
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v != math.Trunc(v) || v > float64(math.MaxInt64/int64(time.Millisecond)) {
			return 0, false
		}
		raw = strconv.FormatFloat(v, 'f', 0, 64)
	default:
		return 0, false
	}
	n, ok := Decimal(raw)
	return n, ok && n <= int64(math.MaxInt64/time.Millisecond)
}

// unusableNameRune bans controls, line/paragraph separators and format
// characters, except ZWNJ/ZWJ, which ordinary Persian and emoji names need.
// Bidi embeddings, overrides and isolates (all Cf) stay banned: they can
// visually reorder the disclosure text around the name.
func unusableNameRune(r rune) bool {
	if r == '\u200c' || r == '\u200d' {
		return false
	}
	return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)
}

// Name is presentation only. Unusable input never becomes an identifier/email
// lookup, and invalid names use the exact ratified generic fallback.
func Name(raw, fallback string) string {
	if !utf8.ValidString(raw) || len(raw) > 128*utf8.UTFMax || strings.IndexFunc(raw, unusableNameRune) >= 0 {
		return fallback
	}
	n := strings.Join(strings.Fields(raw), " ")
	if n == "" || utf8.RuneCountInString(n) > 128 {
		return fallback
	}
	return n
}

func Announcement(bot, owner string) string {
	return fmt.Sprintf("Hi, I'm %s. I'm recording audio and transcribing this meeting for %s. Anyone can type /ace leave to remove me, or the host can remove me.", Name(bot, BotFallback), Name(owner, OwnerFallback))
}

// internal/jobs/progress.go
package jobs

import (
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Progress throttle defaults (aceteam#10876 C1, design 2a.3): emit at most one
// update per 2 seconds OR per 1 percent of advance, whichever comes first. The
// FIRST update and any Final update always pass, so even a one-file index shows
// a line and the UI always lands on the true end state.
const (
	defaultProgressMinInterval     = 2 * time.Second
	defaultProgressMinPercentDelta = 1
)

// NewThrottledProgress wraps emit with the progress throttle and derives the
// wire fields (percent, rate, eta_seconds) emit ships. The returned function is
// what a caller assigns to JobContext.Progress; the handler then calls
// ctx.EmitProgress unconditionally and this throttles.
//
// emit receives the final map[string]any event (ready to publish or print). A
// nil emit yields a no-op (so a caller with no sink can wire it unconditionally).
// now defaults to time.Now; tests inject a fake clock for deterministic
// throttling without sleeping.
func NewThrottledProgress(emit func(map[string]any), now func() time.Time) func(ProgressEvent) {
	if emit == nil {
		return func(ProgressEvent) {}
	}
	if now == nil {
		now = time.Now
	}
	start := now()
	var (
		haveEmitted   bool
		lastEmit      time.Time
		lastPercent   = -1
		lastStage     ProgressStage
		evolvingTotal int
	)
	return func(ev ProgressEvent) {
		t := now()
		minimumTotal := ev.Done + ev.InFlight
		if ev.Total > minimumTotal {
			minimumTotal = ev.Total
		}
		if minimumTotal > evolvingTotal {
			evolvingTotal = minimumTotal
		}
		projectedTotal := evolvingTotal
		if ev.Final {
			projectedTotal = ev.Done
		}
		percent := -1
		if projectedTotal > 0 {
			percent = ev.Done * 100 / projectedTotal
			if ev.Final {
				percent = 100
			} else if percent >= 100 {
				percent = 99
			}
		}
		pass := false
		switch {
		case !haveEmitted:
			pass = true // always emit the first update
		case ev.Final || ev.Flush:
			pass = true // always emit completion or requested partial snapshot
		case ev.Stage != lastStage:
			pass = true // a stage change is always worth surfacing
		case percent >= 0 && lastPercent >= 0 && percent-lastPercent >= defaultProgressMinPercentDelta:
			pass = true
		case t.Sub(lastEmit) >= defaultProgressMinInterval:
			pass = true
		}
		if !pass {
			return
		}
		haveEmitted = true
		lastEmit = t
		lastStage = ev.Stage
		if percent >= 0 {
			lastPercent = percent
		}

		m := map[string]any{
			"stage":   string(ev.Stage),
			"done":    ev.Done,
			"total":   projectedTotal,
			"unit":    ev.Unit,
			"current": sanitizeProgressCurrent(ev.Current),
		}
		if percent >= 0 {
			m["percent"] = percent
		}
		if elapsed := t.Sub(start).Seconds(); elapsed > 0 {
			rate := float64(ev.Done) / elapsed
			m["rate"] = rate
			if rate > 0 && projectedTotal > ev.Done {
				m["eta_seconds"] = int(float64(projectedTotal-ev.Done) / rate)
			}
		}
		if len(ev.Counts) > 0 {
			counts := make(map[string]any, len(ev.Counts))
			for k, v := range ev.Counts {
				counts[k] = v
			}
			m["counts"] = counts
		}
		emit(m)
	}
}

const maxProgressCurrentBytes = 128

// sanitizeProgressCurrent projects an untrusted path/name into bounded display
// metadata. It intentionally does not claim to redact human-readable secrets.
func sanitizeProgressCurrent(value string) string {
	if value == "" {
		return "_"
	}
	value = filepath.Base(value)
	var b strings.Builder
	for len(value) > 0 && b.Len() < maxProgressCurrentBytes {
		r, size := utf8.DecodeRuneInString(value)
		value = value[size:]
		if r == utf8.RuneError && size == 1 {
			r = '_'
		}
		if unicode.IsControl(r) || r == '/' || r == '\\' {
			r = '_'
		}
		need := utf8.RuneLen(r)
		if need < 0 || b.Len()+need > maxProgressCurrentBytes {
			break
		}
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// Package seatquota projects supplied quota observations. It performs no I/O,
// starts no process, grants no admission and never replenishes an allowance.
package seatquota

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"time"
)

const MaxInputBytes = 64 << 10

type Source string

const (
	VendorRead Source = "vendor_read"
	VendorPush Source = "vendor_push"
)
const AdapterVersion = "codex-quota-observation/1"

var ErrInput = errors.New("quota_observation_invalid")
var seatPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// Observation is an inactive proposed quota.reported payload, not a proven
// identity, complete harness-driver event or authorization to route work.
type Observation struct {
	SeatRef               string     `json:"seat_ref"`
	Provider              string     `json:"provider"`
	Window                string     `json:"window,omitempty"`
	WindowMinutes         int        `json:"window_minutes,omitempty"`
	Scope                 string     `json:"scope"`
	Model                 string     `json:"model,omitempty"`
	UsedPercent           *float64   `json:"used_percent"`
	ResetsAt              *time.Time `json:"resets_at"`
	Reached               *bool      `json:"reached"`
	Reason                string     `json:"reason,omitempty"`
	Overage               *bool      `json:"overage"`
	ResetCreditsAvailable *int       `json:"reset_credits_available"`
	Source                Source     `json:"source"`
	Confidence            string     `json:"confidence"`
	AdapterVersion        string     `json:"adapter_version"`
	ObservedAt            time.Time  `json:"observed_at"`
}

// CollectionMethodAllowed is a report-only collection policy, not transport
// enforcement. A future adapter must apply it before sending any request.
func CollectionMethodAllowed(method string) bool { return method == "account/rateLimits/read" }

// Codex accepts the result/params object from a read response or quota push.
// Binding, source and receipt time are caller assertions, not vendor evidence.
// Unknown fields are discarded. Duplicate keys and oversized inputs are refused.
func Codex(raw []byte, seat string, source Source, observed time.Time) ([]Observation, error) {
	if !seatPattern.MatchString(seat) || observed.IsZero() || observed.Year() < 1 || observed.Year() > 9999 || (source != VendorRead && source != VendorPush) || len(raw) == 0 || len(raw) > MaxInputBytes {
		return nil, ErrInput
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	budget := 8192
	if checkJSON(d, 0, &budget) != nil {
		return nil, ErrInput
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, ErrInput
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil || envelope == nil {
		return nil, ErrInput
	}
	snapshot := envelope["rateLimits"]
	if byID, present := envelope["rateLimitsByLimitId"]; present && !bytes.Equal(bytes.TrimSpace(byID), []byte("null")) {
		// Never infer a model or combine buckets as independent capacity.
		// This increment handles only the default Codex metered bucket.
		var buckets map[string]json.RawMessage
		if json.Unmarshal(byID, &buckets) == nil && buckets["codex"] != nil {
			snapshot = buckets["codex"]
		} else {
			snapshot = nil
		}
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(snapshot, &fields) != nil {
		fields = nil
	}
	var credit struct {
		Available *int `json:"availableCount"`
	}
	if json.Unmarshal(envelope["rateLimitResetCredits"], &credit) != nil || credit.Available != nil && (*credit.Available < 0 || *credit.Available > 1000000) {
		credit.Available = nil
	}
	var reached string
	if value, present := fields["rateLimitReachedType"]; present && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		if json.Unmarshal(value, &reached) != nil {
			reached = "unprojected"
		}
	}
	rows := make([]Observation, 0, 2)
	for _, slot := range []string{"primary", "secondary"} {
		row := Observation{SeatRef: seat, Provider: "codex", Scope: "account", Source: source, Confidence: "unknown", AdapterVersion: AdapterVersion, ObservedAt: observed.UTC()}
		if reached == slot {
			flag := true
			row.Reached = &flag
		}
		if credit.Available != nil {
			count := *credit.Available
			row.ResetCreditsAvailable = &count
		}
		var window struct {
			Used    *float64 `json:"usedPercent"`
			Minutes *int     `json:"windowDurationMins"`
			Reset   *int64   `json:"resetsAt"`
		}
		if json.Unmarshal(fields[slot], &window) != nil || window.Minutes == nil || window.Used == nil || *window.Used < 0 || *window.Used > 100 {
			row.Reason = "quota_unknown"
			rows = append(rows, row)
			continue
		}
		if reached != "" && reached != "primary" && reached != "secondary" {
			row.Reason = "vendor_limit_reported"
			rows = append(rows, row)
			continue
		}
		switch *window.Minutes {
		case 300:
			row.Window = "five_hour"
		case 10080:
			row.Window = "seven_day"
		default:
			row.Reason = "unsupported_window"
			rows = append(rows, row)
			continue
		}
		row.WindowMinutes = *window.Minutes
		used := *window.Used
		row.UsedPercent = &used
		row.Confidence = "exact"
		isReached := used >= 100 || reached == slot
		row.Reached = &isReached
		if reached == slot {
			row.Reason = "vendor_window_reached"
		} else if isReached {
			row.Reason = "window_exhausted"
		}
		if window.Reset != nil && *window.Reset > 0 && *window.Reset <= 253402300799 {
			reset := time.Unix(*window.Reset, 0).UTC()
			row.ResetsAt = &reset
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// checkJSON bounds structural work and refuses duplicate keys even in unknown
// optional blocks. Parser diagnostics and vendor strings never escape.
func checkJSON(d *json.Decoder, depth int, budget *int) error {
	*budget--
	if depth > 16 || *budget < 0 {
		return ErrInput
	}
	token, err := d.Token()
	if err != nil {
		return ErrInput
	}
	delim, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			*budget--
			if *budget < 0 {
				return ErrInput
			}
			key, err := d.Token()
			if err != nil {
				return ErrInput
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return ErrInput
			}
			seen[name] = true
			if checkJSON(d, depth+1, budget) != nil {
				return ErrInput
			}
		}
	case '[':
		for d.More() {
			if checkJSON(d, depth+1, budget) != nil {
				return ErrInput
			}
		}
	default:
		return ErrInput
	}
	end, err := d.Token()
	if err != nil || delim == '{' && end != json.Delim('}') || delim == '[' && end != json.Delim(']') {
		return ErrInput
	}
	return nil
}

// Freshness classifies observations only. A reset never implies restored quota.
func Freshness(o Observation, now time.Time) string {
	if now.IsZero() || o.ObservedAt.IsZero() || o.ObservedAt.After(now) || o.Confidence != "exact" || o.UsedPercent == nil || math.IsNaN(*o.UsedPercent) || math.IsInf(*o.UsedPercent, 0) || *o.UsedPercent < 0 || *o.UsedPercent > 100 {
		return "unknown"
	}
	if o.ResetsAt != nil && !o.ResetsAt.After(now) {
		return "quota_reset_unconfirmed"
	}
	age := now.Sub(o.ObservedAt)
	if age <= 120*time.Second {
		return "fresh"
	}
	if age <= 30*time.Minute {
		return "aging"
	}
	return "stale"
}

// Due proposes a free read. The caller must persist every attempt, including
// failure, and supply a trusted clock. No timers, waits or retries run here.
func Due(lastAttempt, lastObservation, now time.Time, onDemand bool, jitter time.Duration) bool {
	if now.IsZero() || jitter < -time.Minute || jitter > time.Minute || lastAttempt.After(now) || lastObservation.After(now) {
		return false
	}
	if !lastAttempt.IsZero() && now.Sub(lastAttempt) < time.Minute {
		return false
	}
	if onDemand {
		return lastObservation.IsZero() || now.Sub(lastObservation) > 120*time.Second
	}
	return lastAttempt.IsZero() || now.Sub(lastAttempt) >= 15*time.Minute+jitter
}

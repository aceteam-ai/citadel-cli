package seatquota

import (
	"regexp"
	"time"
)

var modelPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]{0,127}$`)

// Usage is a supplied consumption projection, not an account allowance. An
// absent actual model is never replaced by a requested model or inferred Auto.
type Usage struct {
	SeatRef      string    `json:"seat_ref"`
	Provider     string    `json:"provider"`
	ModelActual  string    `json:"model_actual,omitempty"`
	InputTokens  *uint64   `json:"input_tokens"`
	OutputTokens *uint64   `json:"output_tokens"`
	Source       string    `json:"source"`
	Confidence   string    `json:"confidence"`
	ObservedAt   time.Time `json:"observed_at"`
}

func Consumption(seat, provider, actual string, input, output *uint64, at time.Time) (Usage, error) {
	if !seatPattern.MatchString(seat) || at.IsZero() || at.Year() < 1 || at.Year() > 9999 || actual != "" && !modelPattern.MatchString(actual) {
		return Usage{}, ErrInput
	}
	switch provider {
	case "codex", "claude", "copilot", "muse":
	default:
		return Usage{}, ErrInput
	}
	u := Usage{SeatRef: seat, Provider: provider, ModelActual: actual, Source: "consumption_only", Confidence: "unknown", ObservedAt: at.UTC()}
	copyCount := func(value *uint64) (*uint64, error) {
		if value == nil {
			return nil, nil
		}
		if *value > 1<<53-1 {
			return nil, ErrInput
		}
		count := *value
		return &count, nil
	}
	var err error
	u.InputTokens, err = copyCount(input)
	if err != nil {
		return Usage{}, err
	}
	u.OutputTokens, err = copyCount(output)
	if err != nil {
		return Usage{}, err
	}
	if input != nil || output != nil {
		u.Confidence = "consumption_only"
	}
	return u, nil
}

// TurnSample has caller-supplied exclusivity and bracketing assertions. The
// collector must establish those independently; this model cannot do so.
type TurnSample struct {
	Before, After      Observation
	StartedAt, EndedAt time.Time
	Exclusive          bool
}
type Estimate struct {
	PercentPerTurn *float64 `json:"est_pct_per_turn"`
	Samples        int      `json:"samples"`
	Confidence     string   `json:"confidence"`
}

// EstimateTurns considers at most sixteen consecutive supplied samples of one
// seat/model/window/reset epoch. It creates no history store or routing action.
func EstimateTurns(samples []TurnSample) (Estimate, error) {
	out := Estimate{Confidence: "unknown"}
	if len(samples) > 16 {
		return out, ErrInput
	}
	var total float64
	var previousEnd time.Time
	var previousUsed *float64
	var baseline *Observation
	for _, sample := range samples {
		a, b := sample.Before, sample.After
		if !sample.Exclusive || sample.StartedAt.IsZero() || sample.EndedAt.Before(sample.StartedAt) || sample.StartedAt.Before(previousEnd) ||
			!seatPattern.MatchString(a.SeatRef) || a.SeatRef != b.SeatRef || a.Provider != b.Provider || a.Scope != b.Scope || a.Model != b.Model || a.Window != b.Window || a.WindowMinutes != b.WindowMinutes ||
			a.ResetsAt == nil || b.ResetsAt == nil || !a.ResetsAt.Equal(*b.ResetsAt) || !b.ResetsAt.After(b.ObservedAt) ||
			a.ObservedAt.After(sample.StartedAt) || b.ObservedAt.Before(sample.EndedAt) || sample.StartedAt.Sub(a.ObservedAt) > 120*time.Second || b.ObservedAt.Sub(sample.EndedAt) > 120*time.Second ||
			Freshness(a, sample.StartedAt) != "fresh" || Freshness(b, b.ObservedAt) != "fresh" || *b.UsedPercent < *a.UsedPercent {
			return out, nil
		}
		if previousUsed != nil && *a.UsedPercent < *previousUsed {
			return out, nil
		}
		if baseline != nil && (a.SeatRef != baseline.SeatRef || a.Provider != baseline.Provider || a.Scope != baseline.Scope || a.Model != baseline.Model || a.Window != baseline.Window || a.WindowMinutes != baseline.WindowMinutes || !a.ResetsAt.Equal(*baseline.ResetsAt)) {
			return out, nil
		}
		if baseline == nil {
			copy := a
			baseline = &copy
		}
		total += *b.UsedPercent - *a.UsedPercent
		previousEnd = sample.EndedAt
		value := *b.UsedPercent
		previousUsed = &value
	}
	if len(samples) > 0 {
		mean := total / float64(len(samples))
		out.PercentPerTurn = &mean
		out.Samples = len(samples)
		out.Confidence = "derived"
	}
	return out, nil
}

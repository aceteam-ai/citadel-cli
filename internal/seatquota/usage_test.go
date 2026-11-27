package seatquota

import (
	"testing"
	"time"
)

func TestSeatQuotaConsumptionIsNotAllowance(t *testing.T) {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, provider := range []string{"codex", "claude", "copilot", "muse"} {
		u, err := Consumption("seat-a", provider, "", nil, nil, at)
		if err != nil || u.Confidence != "unknown" || u.ModelActual != "" || u.InputTokens != nil {
			t.Fatal("unknown consumption changed meaning")
		}
		count := uint64(0)
		u, err = Consumption("seat-a", provider, "model-a", &count, nil, at)
		if err != nil || u.Confidence != "consumption_only" || *u.InputTokens != 0 {
			t.Fatal("explicit consumption zero lost")
		}
		count = 99
		if *u.InputTokens != 0 {
			t.Fatal("mutable input leaked into report")
		}
	}
	large := uint64(1 << 53)
	if _, err := Consumption("seat-a", "codex", "model-a", &large, nil, at); err != ErrInput {
		t.Fatal("unsafe numeric count accepted")
	}
	if _, err := Consumption("seat-a", "codex", "model-a\nextra", nil, nil, at); err != ErrInput {
		t.Fatal("unbounded actual-model label accepted")
	}
}
func TestSeatQuotaTurnEstimateRequiresBracketsAndSameEpoch(t *testing.T) {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	reset := at.Add(time.Hour)
	before, after := 25.0, 30.0
	a := Observation{SeatRef: "seat-a", Provider: "codex", Scope: "account", Model: "model-a", Window: "five_hour", WindowMinutes: 300, UsedPercent: &before, ResetsAt: &reset, ObservedAt: at, Confidence: "exact"}
	b := a
	b.UsedPercent = &after
	b.ObservedAt = at.Add(time.Minute)
	sample := TurnSample{Before: a, After: b, StartedAt: at.Add(time.Second), EndedAt: at.Add(59 * time.Second), Exclusive: true}
	e, err := EstimateTurns([]TurnSample{sample})
	if err != nil || e.PercentPerTurn == nil || *e.PercentPerTurn != 5 || e.Confidence != "derived" {
		t.Fatal("bracket estimate failed")
	}
	for _, change := range []func(*TurnSample){
		func(s *TurnSample) { s.Exclusive = false },
		func(s *TurnSample) { s.After.SeatRef = "seat-b" },
		func(s *TurnSample) { s.After.Model = "model-b" },
		func(s *TurnSample) { next := reset.Add(time.Hour); s.After.ResetsAt = &next },
		func(s *TurnSample) { lower := 20.0; s.After.UsedPercent = &lower },
		func(s *TurnSample) { s.StartedAt = at.Add(-time.Second) },
	} {
		changed := sample
		change(&changed)
		got, err := EstimateTurns([]TurnSample{changed})
		if err != nil || got.PercentPerTurn != nil || got.Confidence != "unknown" {
			t.Fatal("ambiguous sample estimated capacity")
		}
	}
}

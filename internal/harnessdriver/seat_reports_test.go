package harnessdriver

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/seatquota"
)

func seatQuotaFixture() map[string]any {
	return map[string]any{
		"seat_ref": "synthetic-seat", "window": "five_hour", "window_minutes": 300,
		"scope": "account", "used_percent": 0, "resets_at": "2026-01-01T05:00:00Z",
		"reached": false, "overage": nil, "reset_credits_available": 0,
		"source": "vendor_read", "confidence": "exact", "adapter_version": "synthetic/1",
		"observed_at": "2026-01-01T00:00:00Z",
	}
}

func validateSeatFixture(kind string, payload any) error {
	e := sampleEvent(1)
	e.Type = kind
	var err error
	e.Data, err = json.Marshal(payload)
	if err != nil {
		return err
	}
	return e.Validate()
}

func TestSeatQuotaContract(t *testing.T) {
	if err := validateSeatFixture("quota.reported", seatQuotaFixture()); err != nil {
		t.Fatalf("additive seat observation rejected: %v", err)
	}
	for _, window := range []string{"unknown", "five_hour", "seven_day", "weekly", "session", "plan_period", "credits"} {
		p := seatQuotaFixture()
		p["window"], p["used_percent"], p["confidence"] = window, nil, "unknown"
		delete(p, "window_minutes")
		if err := validateSeatFixture("quota.reported", p); err != nil {
			t.Fatalf("unknown %s: %v", window, err)
		}
	}
	for _, change := range []func(map[string]any){
		func(p map[string]any) { delete(p, "seat_ref") },
		func(p map[string]any) { delete(p, "observed_at") },
		func(p map[string]any) { delete(p, "confidence") },
		func(p map[string]any) { p["seat_ref"] = "../seat" },
		func(p map[string]any) { p["source"] = "guessed" },
		func(p map[string]any) { p["confidence"] = "unknown" },
		func(p map[string]any) { p["window"] = "unknown" },
		func(p map[string]any) { p["scope"] = "model" },
		func(p map[string]any) { p["model"] = "invented-model" },
		func(p map[string]any) { p["used_percent"] = 101 },
		func(p map[string]any) { p["used_percent"] = nil },
		func(p map[string]any) { p["source"] = "consumption_only" },
		func(p map[string]any) { p["window_minutes"] = -1 },
		func(p map[string]any) { p["window_minutes"] = 5 },
		func(p map[string]any) { p["reset_credits_available"] = -1 },
		func(p map[string]any) { p["observed_at"] = "invalid" },
		func(p map[string]any) { p["reason"] = "raw vendor message" },
		func(p map[string]any) { p["credentials"] = "not-a-field" },
	} {
		p := seatQuotaFixture()
		change(p)
		if err := validateSeatFixture("quota.reported", p); !errors.Is(err, ErrProtocol) {
			t.Fatalf("invalid seat observation accepted: %v (%v)", p, err)
		}
	}
	p := seatQuotaFixture()
	p["scope"], p["model"] = "model", "synthetic:model"
	if err := validateSeatFixture("quota.reported", p); err != nil {
		t.Fatal(err)
	}
}

func TestSeatUsageContract(t *testing.T) {
	p := map[string]any{"model": "", "model_actual": "synthetic:model", "seat_ref": "synthetic-seat", "input_tokens": nil, "output_tokens": 0, "cached_tokens": nil}
	if err := validateSeatFixture("usage.reported", p); err != nil {
		t.Fatalf("seat consumption rejected: %v", err)
	}
	delete(p, "seat_ref")
	if err := validateSeatFixture("usage.reported", p); !errors.Is(err, ErrProtocol) {
		t.Fatal("unbound actual-model extension accepted")
	}
}

func TestProjectSeatQuotaPreservesUnknownAndOwnsPointers(t *testing.T) {
	now := sampleEvent(1).TS
	raw := []byte(`{"rateLimits":{"primary":{"usedPercent":0,"windowDurationMins":300,"resetsAt":1767243600},"secondary":{"usedPercent":"bad"}},"rateLimitResetCredits":{"availableCount":0,"credits":[{"title":"not projected"}]}}`)
	rows, err := seatquota.Codex(raw, "synthetic-seat", seatquota.VendorRead, now)
	if err != nil {
		t.Fatal(err)
	}
	first, err := ProjectSeatQuota(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	second, err := ProjectSeatQuota(rows[1])
	if err != nil {
		t.Fatal(err)
	}
	if first.UsedPercent == nil || *first.UsedPercent != 0 || first.Reached == nil || *first.Reached || first.ResetCreditsAvailable == nil || *first.ResetCreditsAvailable != 0 || first.Confidence != "exact" {
		t.Fatalf("explicit zero/false lost: %+v", first)
	}
	if second.Window != "unknown" || second.UsedPercent != nil || second.Confidence != "unknown" || second.Overage != nil || second.Model != "" {
		t.Fatalf("unknown replaced: %+v", second)
	}
	for _, p := range []SeatQuota{first, second} {
		if err := validateSeatFixture("quota.reported", p); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(p)
		var fields map[string]any
		_ = json.Unmarshal(encoded, &fields)
		for _, key := range []string{"provider", "credits", "title"} {
			if _, exists := fields[key]; exists {
				t.Fatalf("vendor detail escaped: %s", key)
			}
		}
	}
	*rows[0].UsedPercent = 99
	*rows[0].Reached = true
	*rows[0].ResetCreditsAvailable = 9
	*rows[0].ResetsAt = now
	if *first.UsedPercent != 0 || *first.Reached || *first.ResetCreditsAvailable != 0 || !first.ResetsAt.After(now) {
		t.Fatal("projected report aliases caller pointers")
	}
	if seatquota.Freshness(rows[1], now) != "unknown" {
		t.Fatal("projection changed source unknown policy")
	}
}

func TestProjectSeatReportsRefuseInvalidInputs(t *testing.T) {
	now := sampleEvent(1).TS
	rows, _ := seatquota.Codex([]byte(`{}`), "synthetic-seat", seatquota.VendorRead, now)
	for _, change := range []func(*seatquota.Observation){
		func(o *seatquota.Observation) { o.SeatRef = "bad/seat" },
		func(o *seatquota.Observation) { o.Provider = "unknown" },
		func(o *seatquota.Observation) { o.ObservedAt = time.Time{} },
		func(o *seatquota.Observation) { o.ObservedAt = now.AddDate(10000, 0, 0) },
		func(o *seatquota.Observation) { o.Confidence = "exact" },
		func(o *seatquota.Observation) { n := math.NaN(); o.UsedPercent = &n },
		func(o *seatquota.Observation) { o.AdapterVersion = "" },
	} {
		o := rows[0]
		change(&o)
		got, err := ProjectSeatQuota(o)
		if !errors.Is(err, ErrProtocol) || got.SeatRef != "" || got.UsedPercent != nil {
			t.Fatalf("invalid observation returned partial data: %+v %v", got, err)
		}
	}
	u := seatquota.Usage{SeatRef: "synthetic-seat", Provider: "codex", ObservedAt: now}
	for _, change := range []func(*seatquota.Usage){
		func(u *seatquota.Usage) { u.ModelActual = "raw model text" },
		func(u *seatquota.Usage) { n := uint64(1 << 53); u.InputTokens = &n },
		func(u *seatquota.Usage) { u.SeatRef = "" },
		func(u *seatquota.Usage) { u.ObservedAt = time.Time{} },
	} {
		bad := u
		change(&bad)
		got, err := ProjectSeatUsage(bad)
		if !errors.Is(err, ErrProtocol) || got.SeatRef != "" || got.InputTokens != nil {
			t.Fatalf("invalid consumption returned partial data: %+v %v", got, err)
		}
	}
}

func TestProjectSeatUsageNeverFillsActualModel(t *testing.T) {
	zero := uint64(0)
	u, _ := seatquota.Consumption("synthetic-seat", "codex", "", nil, &zero, sampleEvent(1).TS)
	p, err := ProjectSeatUsage(u)
	if err != nil {
		t.Fatal(err)
	}
	if p.Model != "" || p.ModelActual != "" || p.InputTokens != nil || p.OutputTokens == nil || *p.OutputTokens != 0 || p.CachedTokens != nil {
		t.Fatalf("unknown consumption inferred: %+v", p)
	}
	*u.OutputTokens = 42
	if *p.OutputTokens != 0 {
		t.Fatal("consumption pointer aliases source")
	}
	u.ModelActual = "synthetic:model"
	p, err = ProjectSeatUsage(u)
	if err != nil || p.ModelActual != u.ModelActual || p.Model != "" {
		t.Fatalf("actual model not preserved: %+v %v", p, err)
	}
}

func TestSeatReportsLeaveLegacyEncodingUnchanged(t *testing.T) {
	for _, tc := range []struct {
		payload any
		want    string
	}{
		{Usage{}, `{"model":"","input_tokens":null,"output_tokens":null,"cached_tokens":null}`},
		{Quota{Window: "unknown"}, `{"window":"unknown","used_percent":null,"resets_at":null}`},
	} {
		raw, err := json.Marshal(tc.payload)
		if err != nil || string(raw) != tc.want {
			t.Fatalf("legacy encoding changed: %s %v", raw, err)
		}
	}
}

func TestSeatQuotaProjectionDoesNotReplenishPastReset(t *testing.T) {
	now := sampleEvent(1).TS
	rows, _ := seatquota.Codex([]byte(`{"rateLimits":{"primary":{"usedPercent":90,"windowDurationMins":300,"resetsAt":1767225600}}}`), "synthetic-seat", seatquota.VendorPush, now)
	p, err := ProjectSeatQuota(rows[0])
	if err != nil || p.UsedPercent == nil || *p.UsedPercent != 90 || p.ResetsAt == nil || !p.ResetsAt.Equal(now) {
		t.Fatalf("past reset changed allowance: %+v %v", p, err)
	}
	if seatquota.Freshness(rows[0], now) != "quota_reset_unconfirmed" {
		t.Fatal("past reset no longer requires confirmation")
	}
}

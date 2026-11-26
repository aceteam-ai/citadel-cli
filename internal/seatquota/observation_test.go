package seatquota

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSeatQuotaCodexProjection(t *testing.T) {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	report, err := Codex([]byte(`{"rateLimits":{"primary":{"usedPercent":25,"windowDurationMins":300,"resetsAt":1893459600},"secondary":{"usedPercent":80,"windowDurationMins":10080,"resetsAt":1894060800}},"rateLimitResetCredits":{"availableCount":2,"credits":[{"id":"not-published"}]},"private_extra":"not-published"}`), "seat-a", VendorRead, at)
	if err != nil || len(report) != 2 {
		t.Fatalf("projection failed: %v", err)
	}
	if report[0].Window != "five_hour" || report[1].Window != "seven_day" || report[0].UsedPercent == nil || *report[0].UsedPercent != 25 || report[0].ResetCreditsAvailable == nil || *report[0].ResetCreditsAvailable != 2 {
		t.Fatal("window/percent/reset projection missing")
	}
	raw, _ := json.Marshal(report)
	if strings.Contains(string(raw), "not-published") {
		t.Fatal("unknown vendor data serialized")
	}
	if report[0].SeatRef != "seat-a" || report[0].Confidence != "exact" || report[0].Source != VendorRead {
		t.Fatal("binding/provenance missing")
	}
}
func TestSeatQuotaUnknownIsNotZero(t *testing.T) {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, raw := range []string{`{}`, `{"rateLimits":{"primary":{}}}`, `{"rateLimits":{"primary":{"usedPercent":null,"windowDurationMins":300}}}`, `{"rateLimits":{"primary":{"usedPercent":0,"windowDurationMins":15}}}`} {
		report, err := Codex([]byte(raw), "seat-a", VendorPush, at)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range report {
			if row.UsedPercent != nil || row.Confidence != "unknown" {
				t.Fatal("unknown became available quota")
			}
		}
	}
	r, err := Codex([]byte(`{"rateLimits":{"primary":{"usedPercent":0,"windowDurationMins":300}}}`), "seat-a", VendorRead, at)
	if err != nil || r[0].UsedPercent == nil || *r[0].UsedPercent != 0 {
		t.Fatal("explicit zero lost")
	}
}
func TestSeatQuotaOptionalFailureAndMethodPolicy(t *testing.T) {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	r, err := Codex([]byte(`{"rateLimits":{"primary":{"usedPercent":42,"windowDurationMins":300},"secondary":"malformed"},"rateLimitResetCredits":"malformed"}`), "seat-a", VendorRead, at)
	if err != nil || len(r) != 2 || r[0].UsedPercent == nil || *r[0].UsedPercent != 42 || r[1].Confidence != "unknown" || r[0].ResetCreditsAvailable != nil {
		t.Fatal("optional block discarded independent observation")
	}
	if !CollectionMethodAllowed("account/rateLimits/read") {
		t.Fatal("read method refused")
	}
	for _, method := range []string{"account/rateLimitResetCredit/consume", "account/sendAddCreditsNudgeEmail", "account/login/start", "account/logout", "turn/start", "account/read", "unknown"} {
		if CollectionMethodAllowed(method) {
			t.Fatal("noncollection method allowed")
		}
	}
}
func TestSeatQuotaFreshnessAndCadence(t *testing.T) {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	r, err := Codex([]byte(`{"rateLimits":{"primary":{"usedPercent":50,"windowDurationMins":300,"resetsAt":1893459600}}}`), "seat-a", VendorRead, at)
	if err != nil {
		t.Fatal(err)
	}
	if Freshness(r[0], at.Add(120*time.Second)) != "fresh" || Freshness(r[0], at.Add(121*time.Second)) != "aging" || Freshness(r[0], at.Add(31*time.Minute)) != "stale" {
		t.Fatal("freshness boundaries changed")
	}
	reset := at.Add(-time.Second)
	r[0].ResetsAt = &reset
	if Freshness(r[0], at) != "quota_reset_unconfirmed" {
		t.Fatal("past reset implied replenishment")
	}
	if Due(at, at, at.Add(59*time.Second), true, 0) || !Due(at, at, at.Add(121*time.Second), true, 0) || Due(at, at, at.Add(14*time.Minute), false, 0) || !Due(at, at, at.Add(16*time.Minute), false, 0) {
		t.Fatal("read floor/interval changed")
	}
	if Due(at.Add(time.Minute), at, at, true, 0) {
		t.Fatal("future attempt admitted")
	}
}
func TestSeatQuotaInvalidInputs(t *testing.T) {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, raw := range []string{`[]`, `{"rateLimits":{"primary":{"usedPercent":101,"windowDurationMins":300}}}`, `{"rateLimits":{"primary":{"usedPercent":-1,"windowDurationMins":300}}}`, strings.Repeat("x", MaxInputBytes+1)} {
		r, err := Codex([]byte(raw), "seat-a", VendorRead, at)
		if err == nil {
			for _, row := range r {
				if row.Confidence != "unknown" || row.UsedPercent != nil {
					t.Fatal("invalid value retained quota")
				}
			}
		}
	}
	if _, err := Codex([]byte(`{}`), "../seat", VendorRead, at); err == nil {
		t.Fatal("invalid binding accepted")
	}
}

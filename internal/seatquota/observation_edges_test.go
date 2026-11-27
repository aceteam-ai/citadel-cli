package seatquota

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestSeatQuotaMultipleBucketsNeverMultiplyCapacity(t *testing.T) {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	r, err := Codex([]byte(`{"rateLimits":{"primary":{"usedPercent":1,"windowDurationMins":300}},"rateLimitsByLimitId":{"codex":{"primary":{"usedPercent":80,"windowDurationMins":300}},"another":{"primary":{"usedPercent":0,"windowDurationMins":300}}}}`), "seat-a", VendorRead, at)
	if err != nil || len(r) != 2 || r[0].UsedPercent == nil || *r[0].UsedPercent != 80 {
		t.Fatal("multi-bucket projection changed or multiplied capacity")
	}
	r, err = Codex([]byte(`{"rateLimits":{"primary":{"usedPercent":1,"windowDurationMins":300}},"rateLimitsByLimitId":{"another":{}}}`), "seat-a", VendorRead, at)
	if err != nil || r[0].UsedPercent != nil {
		t.Fatal("unsupported bucket used compatibility capacity")
	}
}
func TestSeatQuotaDuplicateDepthAndProvenanceBounds(t *testing.T) {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, raw := range []string{`{"rateLimits":{},"rateLimits":{}}`, `{"extra":{"x":1,"x":2}}`, strings.Repeat("[", 18) + "0" + strings.Repeat("]", 18), `{} {}`} {
		if _, err := Codex([]byte(raw), "seat-a", VendorRead, at); err != ErrInput {
			t.Fatal("ambiguous or over-deep observation accepted")
		}
	}
	if _, err := Codex([]byte(`{}`), "seat-a", Source("error_message"), at); err != ErrInput {
		t.Fatal("incorrect source assertion accepted")
	}
	if _, err := Codex([]byte(`{}`), "seat-a", VendorRead, time.Time{}); err != ErrInput {
		t.Fatal("missing receipt time accepted")
	}
}
func TestSeatQuotaVendorLimitAndPointerIsolation(t *testing.T) {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	r, err := Codex([]byte(`{"rateLimits":{"primary":{"usedPercent":5,"windowDurationMins":300},"secondary":{"usedPercent":6,"windowDurationMins":10080},"rateLimitReachedType":"credits"},"rateLimitResetCredits":{"availableCount":1}}`), "seat-a", VendorRead, at)
	if err != nil || r[0].Confidence != "unknown" || r[0].UsedPercent != nil || r[0].Reason != "vendor_limit_reported" {
		t.Fatal("unprojected vendor limit became available quota")
	}
	*r[0].ResetCreditsAvailable = 9
	if *r[1].ResetCreditsAvailable != 1 {
		t.Fatal("output rows share a mutable count")
	}
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), `"used_percent":0`) {
		t.Fatal("unknown serialized as zero")
	}
}
func TestSeatQuotaFreshnessRejectsInvalidPercent(t *testing.T) {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, used := range []float64{-1, 101, math.NaN(), math.Inf(1)} {
		if Freshness(Observation{ObservedAt: at, Confidence: "exact", UsedPercent: &used}, at) != "unknown" {
			t.Fatal("invalid supplied percentage classified as fresh")
		}
	}
}

func TestSeatQuotaMalformedLimitStateDoesNotMeanUnlimited(t *testing.T) {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	r, err := Codex([]byte(`{"rateLimits":{"primary":{"usedPercent":5,"windowDurationMins":300},"rateLimitReachedType":42}}`), "seat-a", VendorRead, at)
	if err != nil || r[0].UsedPercent != nil || r[0].Confidence != "unknown" {
		t.Fatal("malformed vendor limit state cleared allowance uncertainty")
	}
	r, err = Codex([]byte(`{"rateLimits":{"primary":{},"rateLimitReachedType":"primary"}}`), "seat-a", VendorRead, at)
	if err != nil || r[0].Reached == nil || !*r[0].Reached || r[0].UsedPercent != nil {
		t.Fatal("known reached state lost with incomplete percentage")
	}
}

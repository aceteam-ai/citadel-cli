package footprint

import (
	"testing"
	"time"
)

func TestIntervalFromEnv(t *testing.T) {
	cases := map[string]struct {
		want    time.Duration
		enabled bool
	}{
		"":     {DefaultInterval, true},
		"30":   {30 * time.Second, true},
		"0":    {0, false},
		"-5":   {0, false},
		"junk": {DefaultInterval, true},
	}
	for raw, exp := range cases {
		t.Setenv("CITADEL_FOOTPRINT_INTERVAL", raw)
		if raw == "" {
			// t.Setenv can't unset; emulate unset by clearing.
			t.Setenv("CITADEL_FOOTPRINT_INTERVAL", "")
		}
		got, enabled := IntervalFromEnv()
		if enabled != exp.enabled || (enabled && got != exp.want) {
			t.Errorf("IntervalFromEnv(%q) = (%s, %v), want (%s, %v)", raw, got, enabled, exp.want, exp.enabled)
		}
	}
}

func TestRetentionFromEnv(t *testing.T) {
	t.Setenv("CITADEL_FOOTPRINT_RETENTION_DAYS", "")
	if got := RetentionFromEnv(); got != DefaultRetentionDays {
		t.Errorf("unset retention = %d, want %d", got, DefaultRetentionDays)
	}
	t.Setenv("CITADEL_FOOTPRINT_RETENTION_DAYS", "14")
	if got := RetentionFromEnv(); got != 14 {
		t.Errorf("retention = %d, want 14", got)
	}
	t.Setenv("CITADEL_FOOTPRINT_RETENTION_DAYS", "junk")
	if got := RetentionFromEnv(); got != DefaultRetentionDays {
		t.Errorf("junk retention = %d, want default %d", got, DefaultRetentionDays)
	}
}

func TestDefaultDir(t *testing.T) {
	if got := DefaultDir("/home/x/citadel-node"); got != "/home/x/citadel-node/footprints" {
		t.Errorf("DefaultDir = %q", got)
	}
}

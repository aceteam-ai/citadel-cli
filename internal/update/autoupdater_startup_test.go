package update

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestRunLogsEffectiveStartupPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, source, want string
		enabled            bool
		legacy, noCallback bool
	}{
		{name: "persisted enabled", enabled: true, source: "persisted", want: "auto-update: enabled (persisted), checking every 1h0m0s"},
		{name: "persisted disabled", source: "persisted", want: "auto-update: disabled (persisted); loop will re-check the policy every 1h0m0s"},
		{name: "environment enabled", enabled: true, source: "env", want: "auto-update: enabled (env), checking every 1h0m0s"},
		{name: "unreadable preference", source: "unknown", want: "auto-update: disabled (unknown); loop will re-check the policy every 1h0m0s"},
		{name: "empty source", source: "", want: "auto-update: disabled (unspecified); loop will re-check the policy every 1h0m0s"},
		{name: "legacy disabled callback", legacy: true, want: "auto-update: disabled (callback); loop will re-check the policy every 1h0m0s"},
		{name: "no callback preserves enabled default", noCallback: true, want: "auto-update: enabled (default), checking every 1h0m0s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checker := &fakeChecker{}
			var lines []string
			policyCalls, enabledCalls, refreshCalls := 0, 0, 0
			cfg := AutoUpdaterConfig{
				Checker: checker,
				Ticks:   make(chan time.Time),
				Log: func(format string, args ...any) {
					lines = append(lines, fmt.Sprintf(format, args...))
				},
				Refresh: func() { refreshCalls++ },
			}
			if !tc.noCallback {
				cfg.Enabled = func() bool { enabledCalls++; return tc.enabled }
				if !tc.legacy {
					cfg.Policy = func() (bool, string) {
						policyCalls++
						return tc.enabled, tc.source
					}
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			NewAutoUpdater(cfg).Run(ctx)
			if len(lines) != 1 || lines[0] != tc.want {
				t.Fatalf("startup log = %q, want exactly %q", lines, tc.want)
			}
			wantPolicy, wantEnabled := 1, 0
			if tc.legacy {
				wantPolicy, wantEnabled = 0, 1
			} else if tc.noCallback {
				wantPolicy = 0
			}
			if policyCalls != wantPolicy || enabledCalls != wantEnabled {
				t.Fatalf("policy calls=%d enabled calls=%d, want %d/%d", policyCalls, enabledCalls, wantPolicy, wantEnabled)
			}
			if refreshCalls != 1 || checker.checkCalls != 0 || checker.downloadHits != 0 {
				t.Fatalf("startup changed reporting or performed an update: refresh=%d checks=%d downloads=%d", refreshCalls, checker.checkCalls, checker.downloadHits)
			}
		})
	}
}

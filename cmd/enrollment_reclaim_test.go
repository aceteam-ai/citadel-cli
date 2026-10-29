package cmd

import (
	"reflect"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

func TestEnrollmentReclaimDoesNotInheritReplacementCredentials(t *testing.T) {
	for _, current := range []*DeviceAuthResult{nil, {Token: &nexus.TokenResponse{DeviceAPIToken: "fixture-current"}}} {
		reclaimEnrollmentNodesWith(true, current, func() *DeviceConfig {
			t.Fatal("force-new attempt loaded predecessor credentials")
			return nil
		}, func(string, string) { t.Fatal("force-new attempt reclaimed a predecessor") }, "fixture-host")
	}
}

func TestEnrollmentReclaimUsesOnlyCurrentGrantDespiteSavedState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token *nexus.TokenResponse
		want  int
	}{
		{"current", &nexus.TokenResponse{DeviceAPIToken: "fixture-current"}, 2},
		{"empty-token", &nexus.TokenResponse{}, 0},
		{"missing-token", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hosts []string
			reclaimEnrollmentNodesWith(false, &DeviceAuthResult{Token: tc.token}, func() *DeviceConfig {
				t.Fatal("current grant fell back to a saved credential")
				return &DeviceConfig{DeviceAPIToken: "fixture-old"}
			}, func(token, hostname string) {
				if token != "fixture-current" {
					t.Fatal("reclaim used a different grant")
				}
				hosts = append(hosts, hostname)
			}, "fixture-serving", "fixture-display", "fixture-serving", "")
			if len(hosts) != tc.want {
				t.Fatalf("reclaim count=%d want=%d", len(hosts), tc.want)
			}
			if tc.want != 0 && !reflect.DeepEqual(hosts, []string{"fixture-serving", "fixture-display"}) {
				t.Fatal("reclaim changed or repeated the requested hostnames")
			}
		})
	}
}

func TestEnrollmentReclaimPreservesExplicitLegacySavedCredentialPath(t *testing.T) {
	loads, calls := 0, 0
	reclaimEnrollmentNodesWith(false, nil, func() *DeviceConfig {
		loads++
		return &DeviceConfig{DeviceAPIToken: "fixture-saved"}
	}, func(token, host string) {
		calls++
		if token != "fixture-saved" || host != "fixture-host" {
			t.Fatal("legacy reclaim changed its credential or hostname")
		}
	}, "fixture-host")
	if loads != 1 || calls != 1 {
		t.Fatalf("legacy loads=%d calls=%d", loads, calls)
	}
}

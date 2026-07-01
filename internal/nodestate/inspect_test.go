package nodestate

import (
	"testing"

	fabricpb "github.com/aceteam-ai/fabric-protocol/gen/go/aceteam/fabric/v1"
)

func TestMapStatus(t *testing.T) {
	cases := map[string]fabricpb.ModuleStatus{
		"running":    fabricpb.ModuleStatus_MODULE_STATUS_RUNNING,
		"restarting": fabricpb.ModuleStatus_MODULE_STATUS_RUNNING,
		"exited":     fabricpb.ModuleStatus_MODULE_STATUS_STOPPED,
		"created":    fabricpb.ModuleStatus_MODULE_STATUS_STOPPED,
		"paused":     fabricpb.ModuleStatus_MODULE_STATUS_STOPPED,
		"dead":       fabricpb.ModuleStatus_MODULE_STATUS_STOPPED,
		"":           fabricpb.ModuleStatus_MODULE_STATUS_UNSPECIFIED,
	}
	for in, want := range cases {
		if got := mapStatus(in); got != want {
			t.Errorf("mapStatus(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestMapHealth_HealthcheckWins(t *testing.T) {
	cases := []struct {
		status, health string
		want           fabricpb.ModuleHealth
	}{
		// An explicit healthcheck result is authoritative regardless of status.
		{"running", "healthy", fabricpb.ModuleHealth_MODULE_HEALTH_HEALTHY},
		{"running", "starting", fabricpb.ModuleHealth_MODULE_HEALTH_STARTING},
		{"running", "unhealthy", fabricpb.ModuleHealth_MODULE_HEALTH_UNHEALTHY},
		// No healthcheck: infer from run state.
		{"running", "", fabricpb.ModuleHealth_MODULE_HEALTH_HEALTHY},
		{"restarting", "", fabricpb.ModuleHealth_MODULE_HEALTH_STARTING},
		{"exited", "", fabricpb.ModuleHealth_MODULE_HEALTH_UNHEALTHY},
		{"", "", fabricpb.ModuleHealth_MODULE_HEALTH_UNSPECIFIED},
	}
	for _, tc := range cases {
		if got := mapHealth(tc.status, tc.health); got != tc.want {
			t.Errorf("mapHealth(%q,%q) = %v, want %v", tc.status, tc.health, got, tc.want)
		}
	}
}

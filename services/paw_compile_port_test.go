package services

import "testing"

// The catalog module is not an embedded ServiceMap service. Registration must
// supply its compose variable and keep its default distinct from other owners.
func TestPAWCompileHostPortRegistered(t *testing.T) {
	const wantPort = 8217
	const wantName = "CITADEL_PAW_COMPILE_HOST_PORT"
	if got, ok := ServiceHostPorts["paw-compile"]; !ok || got != wantPort {
		t.Errorf("paw-compile registration = %d (present=%v), want %d", got, ok, wantPort)
	}
	if got, ok := ManagedServiceHostPort("paw-compile"); !ok || got != wantPort {
		t.Errorf("managed paw-compile port = %d (present=%v), want %d", got, ok, wantPort)
	}
	if got, ok := HostPortEnvVarName("paw-compile"); !ok || got != wantName {
		t.Errorf("paw-compile env = %q (present=%v), want %q", got, ok, wantName)
	}
	if wantPort >= AppsPortRangeStart && wantPort <= AppsPortRangeEnd {
		t.Fatal("paw-compile port overlaps automatic app allocation")
	}
	if owner, taken := ReservedCitadelPorts[wantPort]; taken {
		t.Errorf("paw-compile port overlaps reserved surface %q", owner)
	}
	for name, port := range ServiceHostPorts {
		if name != "paw-compile" && port == wantPort {
			t.Errorf("paw-compile port overlaps managed service %q", name)
		}
	}
	found := false
	for _, entry := range HostPortEnv() {
		if entry == wantName+"=8217" {
			found = true
		}
	}
	if !found {
		t.Fatal("HostPortEnv must inject CITADEL_PAW_COMPILE_HOST_PORT=8217")
	}
}

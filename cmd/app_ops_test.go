package cmd

import "testing"

// TestRefuseReservedAppExposeName pins the /agent/expose control-path guard
// (CRAM A1): an operator cannot expose or unexpose a reserved "app-<short_code>"
// route through the local control endpoint. This is the enforcement point the
// EXPOSE_SET/UNEXPOSE job parsers don't reach, so it needs its own pin.
func TestRefuseReservedAppExposeName(t *testing.T) {
	for _, n := range []string{"app-ac-blue-cat-fox", "app-x", "app-"} {
		if err := refuseReservedAppExposeName(n, "pick another name"); err == nil {
			t.Errorf("refuseReservedAppExposeName(%q) = nil, want an error", n)
		}
	}
	for _, n := range []string{"frigate", "grafana", "myapp", ""} {
		if err := refuseReservedAppExposeName(n, "pick another name"); err != nil {
			t.Errorf("refuseReservedAppExposeName(%q) = %v, want nil", n, err)
		}
	}
}

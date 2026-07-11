package cmd

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/services"
)

func TestGatewayVPNBindErrorMessageNamesPortAndRoutes(t *testing.T) {
	msg := gatewayVPNBindErrorMessage(8443, errors.New("listen on 100.64.0.21:8443: connection refused"))

	for _, want := range []string{"8443", "/vnc", "/terminal", "/modules/*", "LAN"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
	// Not a same-process double bind: no collision hint pointing at the control
	// listener.
	if strings.Contains(msg, "CITADEL_CONTROL_PORT") {
		t.Errorf("collision hint should only appear for \"listener already open\" errors:\n%s", msg)
	}
}

func TestGatewayVPNBindErrorMessageCollisionHint(t *testing.T) {
	err := fmt.Errorf("listen on 100.64.0.21:8443: tsnet: listener already open for tcp, 100.64.0.21:8443")
	msg := gatewayVPNBindErrorMessage(8443, err)

	// A same-process collision must name the culprit knob and the control
	// listener's current default so the operator can fix the config without
	// spelunking (#504).
	for _, want := range []string{
		"CITADEL_CONTROL_PORT",
		strconv.Itoa(services.ControlMTLSPort),
		"--gateway-port",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("collision message missing %q:\n%s", want, msg)
		}
	}
}

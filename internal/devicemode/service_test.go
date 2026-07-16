package devicemode

import (
	"strings"
	"testing"
)

func TestLaunchdPlist(t *testing.T) {
	plist := LaunchdPlist("/usr/local/bin/citadel", "/Users/alice")

	for _, want := range []string{
		"<string>" + LaunchdLabel + "</string>",
		"<string>/usr/local/bin/citadel</string>",
		"<string>device</string>",
		"<string>run</string>",
		"<key>KeepAlive</key>",
		"<key>RunAtLoad</key>",
		"/Users/alice/Library/Logs/citadel/citadel-device.log",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q", want)
		}
	}
	// Distinct label from the node-agent service — device mode must be able
	// to coexist with a later device->node flip.
	if strings.Contains(plist, "<string>ai.aceteam.citadel</string>") {
		t.Error("device plist must not reuse the node agent's launchd label")
	}
}

func TestSystemdUnit(t *testing.T) {
	unit := SystemdUnit("/usr/local/bin/citadel")
	for _, want := range []string{
		"ExecStart=/usr/local/bin/citadel device run",
		"Restart=always",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q", want)
		}
	}
}

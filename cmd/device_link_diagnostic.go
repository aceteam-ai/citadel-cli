package cmd

import (
	"fmt"
	"io"
)

// Saved credentials and a connected mesh are not evidence of a completed
// current-key link. There is no authenticated binding-lookup contract yet;
// report uncertainty without replaying grants, deleting keys, or blocking
// compatible legacy/offline startup. See docs/design-incomplete-device-link.md.
const deviceLinkUnverified = "unverified (saved credentials do not prove a completed device link). If enrollment did not finish or the API refuses this node, run 'citadel logout && citadel init' for fresh browser approval."

func deviceLinkDiagnostic(dc *DeviceConfig) string {
	if dc == nil || dc.DeviceAPIToken == "" {
		return ""
	}
	return deviceLinkUnverified
}

func printDeviceLinkDiagnostic(w io.Writer, dc *DeviceConfig) {
	if detail := deviceLinkDiagnostic(dc); detail != "" {
		fmt.Fprintf(w, "  Device link: %s\n", detail)
	}
}

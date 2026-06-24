// internal/ui/qrcode_test.go
package ui

import (
	"strings"
	"testing"
)

func TestBuildEnrollPayload(t *testing.T) {
	tests := []struct {
		name            string
		verificationURI string
		userCode        string
		want            string
	}{
		{
			name:            "bare verification URI gets code and version",
			verificationURI: "https://aceteam.ai/device",
			userCode:        "ABCD-1234",
			want:            "https://aceteam.ai/device?code=ABCD-1234&v=1",
		},
		{
			name:            "already-complete URI gets version appended",
			verificationURI: "https://aceteam.ai/device?code=ABCD-1234",
			userCode:        "ABCD-1234",
			want:            "https://aceteam.ai/device?code=ABCD-1234&v=1",
		},
		{
			name:            "user code is query-escaped",
			verificationURI: "https://aceteam.ai/device",
			userCode:        "AB CD",
			want:            "https://aceteam.ai/device?code=AB+CD&v=1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildEnrollPayload(tt.verificationURI, tt.userCode)
			if got != tt.want {
				t.Errorf("BuildEnrollPayload(%q, %q) = %q, want %q",
					tt.verificationURI, tt.userCode, got, tt.want)
			}
		})
	}
}

// TestBuildEnrollPayloadNeverLeaksDeviceCode is a guardrail: the payload must
// only ever carry the user_code, never the device_code (the node's polling
// secret). This test documents the security invariant.
func TestBuildEnrollPayloadCarriesUserCodeOnly(t *testing.T) {
	payload := BuildEnrollPayload("https://aceteam.ai/device", "USER-CODE-123")
	if !strings.Contains(payload, "code=USER-CODE-123") {
		t.Fatalf("payload missing user code: %q", payload)
	}
	if !strings.Contains(payload, "v=1") {
		t.Fatalf("payload missing version marker: %q", payload)
	}
}

func TestRenderQRCodeProducesScannableBlock(t *testing.T) {
	out := RenderQRCode("https://aceteam.ai/device?code=ABCD-1234&v=1")
	if strings.TrimSpace(out) == "" {
		t.Fatal("RenderQRCode produced empty output")
	}
	// Half-block renderer should emit block runes for the QR modules.
	if !strings.ContainsAny(out, "█▀▄ ") {
		t.Errorf("output does not look like a half-block QR: %q", out)
	}
	// Sanity: a QR for this payload should be a non-trivial multi-line block.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 10 {
		t.Errorf("QR output suspiciously short: %d lines", len(lines))
	}
}

func TestRenderEnrollQR(t *testing.T) {
	out := RenderEnrollQR("https://aceteam.ai/device", "ABCD-1234")
	if strings.TrimSpace(out) == "" {
		t.Fatal("RenderEnrollQR produced empty output")
	}
}

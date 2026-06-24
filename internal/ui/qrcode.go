// internal/ui/qrcode.go
package ui

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"

	"github.com/mdp/qrterminal/v3"
)

// EnrollPayloadVersion is the version of the QR enrollment payload schema.
// It is appended as a "v" query parameter to the enrollment URL so the iOS
// scanner (aceteam-ai/aceteam#4224) can evolve the contract without breaking
// older nodes. Unknown query params are ignored by the existing web approval
// page, so this stays backward compatible.
const EnrollPayloadVersion = "1"

// BuildEnrollPayload returns the string encoded into the enrollment QR code.
//
// The payload is the device-authorization "verification_uri_complete" URL
// (RFC 8628 §3.3.1) with a version marker appended:
//
//	{verificationURI}?code={userCode}&v={EnrollPayloadVersion}
//
// e.g. https://aceteam.ai/device?code=ABCD-1234&v=1
//
// Design notes:
//   - Encodes the user_code, NEVER the device_code. The device_code is the
//     node's bearer secret used to poll /token; placing it in a QR anyone can
//     scan would let them steal the resulting authkey. The user_code is the
//     value the human/app approves, exactly as the web flow uses it.
//   - It is a real, resolvable URL, so a plain phone camera (no Citadel app)
//     opens the working web approval page. The Citadel iOS app (aceteam#4224)
//     can instead parse the `code` param and approve in-app via the org
//     pre-auth-key / approval route (aceteam#3958).
//   - The node continues polling /token with its private device_code; once the
//     scan is approved the node binds to the org Headscale user (org_<id>),
//     authenticated and org-scoped. No new auth protocol is introduced.
func BuildEnrollPayload(verificationURI, userCode string) string {
	// If the caller already passed a complete URL with a code, respect it but
	// ensure the version marker is present.
	base := verificationURI
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}

	// Only append code= when the base does not already carry one.
	if !strings.Contains(base, "code=") && userCode != "" {
		base = fmt.Sprintf("%s%scode=%s", base, sep, url.QueryEscape(userCode))
		sep = "&"
	} else if strings.Contains(base, "?") {
		sep = "&"
	}

	if !strings.Contains(base, "v=") {
		base = fmt.Sprintf("%s%sv=%s", base, sep, EnrollPayloadVersion)
	}
	return base
}

// RenderQRCode renders the given content as a scannable QR code using terminal
// half-block characters and returns it as a string (with a trailing newline).
//
// It uses a quiet zone (the white border required by the QR spec) and the
// compact half-block renderer so a typical payload fits within an 80-column
// terminal. Missing the quiet zone is the most common reason terminal QR codes
// fail to scan, so it is always included.
func RenderQRCode(content string) string {
	var buf bytes.Buffer
	config := qrterminal.Config{
		Level:      qrterminal.M, // medium error correction, good density/robustness
		Writer:     &buf,
		HalfBlocks: true,
		QuietZone:  2,
		// Half-block mode uses foreground/background block runes; the
		// White/Black names refer to QR modules, rendered with ANSI blocks.
		BlackChar:      qrterminal.BLACK_BLACK,
		WhiteChar:      qrterminal.WHITE_WHITE,
		BlackWhiteChar: qrterminal.BLACK_WHITE,
		WhiteBlackChar: qrterminal.WHITE_BLACK,
	}
	qrterminal.GenerateWithConfig(content, config)
	return buf.String()
}

// RenderEnrollQR is a convenience wrapper that builds the enrollment payload
// and renders it as a terminal QR code.
func RenderEnrollQR(verificationURI, userCode string) string {
	return RenderQRCode(BuildEnrollPayload(verificationURI, userCode))
}

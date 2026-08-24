// Package cobrowsestream streams a single interactive browser session (#793) to
// a remote viewer and forwards the viewer's input back to it, carried over the
// node's EXISTING mesh transport (a tsnet VPN listener + WebSocket, the same
// mechanism VNC, the terminal server, and the H.264 deskstream #338 use). No new
// relay or socket infrastructure is stood up: the tsnet mesh (with its DERP
// relay for NAT traversal) is the transport, and the mesh is the trust boundary.
//
// Frames come from CDP Page.startScreencast attached to the session's debug
// port; input is applied via CDP Input.dispatchMouseEvent / dispatchKeyEvent.
// This is the node-side complement of the web viewer (#8132), which speaks the
// wire contract defined here.
package cobrowsestream

import "encoding/json"

// StreamPath is the WebSocket route the server serves. The session id is passed
// as the "id" query parameter (e.g. ws://<mesh-ip>:5911/cobrowse/stream?id=cb-…).
const StreamPath = "/cobrowse/stream"

// HealthPath is a cheap liveness route mirroring the deskstream server.
const HealthPath = "/health"

// --- Server -> viewer -----------------------------------------------------

// InitMessage is the FIRST WebSocket frame the server sends, a TEXT frame with
// this JSON. A viewer MUST read it before decoding any BINARY frame. The wire
// contract is FIXED; do not reorder or rename fields.
//
// Frames that follow are BINARY WebSocket frames, each the raw bytes of one JPEG
// image (the CDP screencast frame, base64-decoded on the node). A viewer renders
// each binary frame directly (e.g. a blob URL), no envelope to parse.
type InitMessage struct {
	Type      string `json:"type"`      // always "init"
	Codec     string `json:"codec"`     // always "mjpeg" (a sequence of standalone JPEGs)
	SessionID string `json:"sessionId"` // the co-browse session being streamed
}

// NewInitMessage builds the init message for a session.
func NewInitMessage(sessionID string) InitMessage {
	return InitMessage{Type: "init", Codec: "mjpeg", SessionID: sessionID}
}

// Marshal returns the JSON bytes for the init TEXT frame.
func (m InitMessage) Marshal() ([]byte, error) { return json.Marshal(m) }

// --- Viewer -> server -----------------------------------------------------

// Input message "type" values a viewer sends as TEXT JSON frames.
const (
	InputTypeMouse = "mouse"
	InputTypeKey   = "key"
)

// InputMessage is a TEXT frame the viewer sends to drive the session. Unknown
// types are ignored by the server so the contract can grow without breaking old
// viewers.
//
// COORDINATE CONTRACT (mouse): X and Y are NORMALIZED to the range [0,1] against
// the streamed frame — X is the fraction from the left edge, Y from the top. The
// server multiplies them by the latest screencast frame's device dimensions to
// get CDP viewport CSS pixels. Sending normalized coords means the viewer never
// needs to know the capture resolution and a resize can never desync input from
// video. Values outside [0,1] are clamped.
type InputMessage struct {
	Type string `json:"type"`

	// Mouse fields (Type == "mouse").
	Event      string  `json:"event,omitempty"`      // CDP: mousePressed|mouseReleased|mouseMoved|mouseWheel
	X          float64 `json:"x,omitempty"`          // normalized [0,1] from left
	Y          float64 `json:"y,omitempty"`          // normalized [0,1] from top
	Button     string  `json:"button,omitempty"`     // none|left|middle|right|back|forward
	Buttons    int     `json:"buttons,omitempty"`    // CDP buttons bitmask
	ClickCount int     `json:"clickCount,omitempty"` // for pressed/released
	DeltaX     float64 `json:"deltaX,omitempty"`     // wheel
	DeltaY     float64 `json:"deltaY,omitempty"`     // wheel

	// Key fields (Type == "key").
	KeyEvent  string `json:"keyEvent,omitempty"`  // CDP: keyDown|keyUp|rawKeyDown|char
	Key       string `json:"key,omitempty"`       // DOM key, e.g. "a", "Enter"
	Code      string `json:"code,omitempty"`      // DOM code, e.g. "KeyA"
	Text      string `json:"text,omitempty"`      // text produced (for keyDown/char)
	KeyCode   int    `json:"keyCode,omitempty"`   // windowsVirtualKeyCode
	Modifiers int    `json:"modifiers,omitempty"` // CDP modifiers bitmask (alt=1,ctrl=2,meta=4,shift=8)
}

// parseInputMessage decodes a viewer TEXT frame. It returns ok=false when the
// frame is not valid JSON or carries no recognized type, so a malformed frame is
// dropped rather than fatal.
func parseInputMessage(data []byte) (InputMessage, bool) {
	var m InputMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return InputMessage{}, false
	}
	if m.Type != InputTypeMouse && m.Type != InputTypeKey {
		return InputMessage{}, false
	}
	return m, true
}

// clamp01 bounds v to [0,1] so an out-of-range coordinate never dispatches an
// off-screen or negative CDP event.
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

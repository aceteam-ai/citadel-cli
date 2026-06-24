package deskstream

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

// InitMessage is the FIRST WebSocket frame the server sends, as a TEXT frame
// containing this JSON. Clients (web #4254, iOS #4255) MUST read it before
// decoding any BINARY frame, since it carries the codec and geometry needed to
// configure their decoder. The wire contract is FIXED; do not reorder or rename
// fields.
type InitMessage struct {
	Type             string `json:"type"`             // always "init"
	Codec            string `json:"codec"`            // always "h264"
	Width            int    `json:"width"`            // frame width in pixels
	Height           int    `json:"height"`           // frame height in pixels
	FPS              int    `json:"fps"`              // target frame rate
	KeyframeInterval int    `json:"keyframeInterval"` // forced IDR interval in frames
}

// NewInitMessage builds the init message for a session.
func NewInitMessage(width, height, fps, keyframeInterval int) InitMessage {
	return InitMessage{
		Type:             "init",
		Codec:            "h264",
		Width:            width,
		Height:           height,
		FPS:              fps,
		KeyframeInterval: keyframeInterval,
	}
}

// Marshal returns the JSON bytes for the init TEXT frame.
func (m InitMessage) Marshal() ([]byte, error) { return json.Marshal(m) }

// ClientMessage is a TEXT frame sent by a client to the server. The only
// recognized type is "requestKeyframe", which forces the server to emit an IDR
// (with SPS+PPS) as soon as possible.
type ClientMessage struct {
	Type string `json:"type"` // "requestKeyframe"
}

// ClientMsgRequestKeyframe is the value of ClientMessage.Type that requests an
// immediate keyframe.
const ClientMsgRequestKeyframe = "requestKeyframe"

// parseClientMessage decodes a client TEXT frame. It returns the message type,
// or "" if the frame is not valid JSON or carries no type.
func parseClientMessage(data []byte) string {
	var m ClientMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	return m.Type
}

// Geometry is the detected desktop resolution.
type Geometry struct {
	Width  int
	Height int
}

// defaultGeometry is used when detection fails so the init message always
// carries plausible dimensions.
var defaultGeometry = Geometry{Width: 1280, Height: 720}

var xdpyResolutionRe = regexp.MustCompile(`dimensions:\s+(\d+)x(\d+)`)

// detectGeometry returns the X display resolution, using xdpyinfo when present.
// It falls back to defaultGeometry when the tool is absent or parsing fails, so
// a stream can still start (ffmpeg's x11grab will grab the real size; the init
// message just advertises a best-effort guess that clients can correct from the
// decoded SPS).
func detectGeometry(display string) Geometry {
	path, err := exec.LookPath("xdpyinfo")
	if err != nil {
		return defaultGeometry
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path)
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	out, err := cmd.Output()
	if err != nil {
		return defaultGeometry
	}
	return parseXdpyinfoGeometry(string(out))
}

// parseXdpyinfoGeometry extracts WxH from xdpyinfo output. Exposed for testing.
func parseXdpyinfoGeometry(out string) Geometry {
	m := xdpyResolutionRe.FindStringSubmatch(out)
	if len(m) != 3 {
		return defaultGeometry
	}
	w, err1 := strconv.Atoi(m[1])
	h, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return defaultGeometry
	}
	return Geometry{Width: w, Height: h}
}

package cobrowsestream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// cdpHTTPClient bounds the /json target probe so a wedged DevTools endpoint
// cannot hang a viewer connection forever.
var cdpHTTPClient = &http.Client{Timeout: 5 * time.Second}

// cdpTargetURL resolves a debug port to the page target's DevTools WebSocket URL.
// It is a package var so tests can point the CDP client at a fake Chrome
// (httptest WebSocket) without launching a real browser -- the same injection
// seam pattern the platform launchers use. Like the existing screenshot path it
// grabs the FIRST page target, so a session that opens a second tab streams the
// first tab (a documented, inherited limitation).
var cdpTargetURL = defaultCDPTargetURL

func defaultCDPTargetURL(debugPort int) (string, error) {
	resp, err := cdpHTTPClient.Get(fmt.Sprintf("http://127.0.0.1:%d/json", debugPort))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var targets []struct {
		Type                 string `json:"type"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &targets); err != nil {
		return "", err
	}
	for _, t := range targets {
		if t.Type == "page" && t.WebSocketDebuggerURL != "" {
			return t.WebSocketDebuggerURL, nil
		}
	}
	return "", fmt.Errorf("no page target found on CDP port %d", debugPort)
}

// screencastFrame is one decoded CDP Page.screencastFrame event.
type screencastFrame struct {
	// jpeg is the raw image bytes (CDP delivers base64; decoded here once).
	jpeg []byte
	// deviceWidth/deviceHeight are the frame's CDP device dimensions, used to map
	// a viewer's normalized [0,1] input coords to CDP viewport CSS pixels.
	deviceWidth  float64
	deviceHeight float64
}

// cdpClient is a persistent CDP WebSocket to one page target. All writes go
// through send() under writeMu because gorilla panics on concurrent writes and
// this conn is written from three places (the screencast ack in the read loop,
// viewer input dispatch, and lifecycle start/stop).
type cdpClient struct {
	conn *websocket.Conn

	writeMu sync.Mutex
	idMu    sync.Mutex
	nextID  int

	// onFrame is invoked from the read loop for every screencast frame. It must
	// not block for long (it feeds the coalescing single-slot holder).
	onFrame func(screencastFrame)

	// metaMu guards the last frame's device dimensions, read by the input path.
	metaMu sync.Mutex
	lastW  float64
	lastH  float64

	closeOnce sync.Once
	done      chan struct{}
}

// dialCDP resolves the page target for debugPort and opens a persistent CDP
// WebSocket. onFrame receives every screencast frame once startScreencast runs.
func dialCDP(debugPort int, onFrame func(screencastFrame)) (*cdpClient, error) {
	wsURL, err := cdpTargetURL(debugPort)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("CDP dial: %w", err)
	}
	c := &cdpClient{conn: conn, onFrame: onFrame, done: make(chan struct{})}
	go c.readLoop()
	return c, nil
}

// send writes one JSON message to the CDP conn under the write lock. It does not
// wait for a response; CDP responses and events are handled in readLoop. This
// fire-and-forget model is sufficient here: screencast is event-driven and input
// dispatch does not need the ack to proceed (local RTT, and dropping the wait
// keeps a flooding viewer from serializing on round-trips).
func (c *cdpClient) send(method string, params map[string]any) error {
	c.idMu.Lock()
	c.nextID++
	id := c.nextID
	c.idMu.Unlock()

	msg := map[string]any{"id": id, "method": method, "params": params}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteJSON(msg)
}

// readLoop dispatches CDP messages until the conn closes. Events (no id) are
// routed by method; the only one we act on is Page.screencastFrame, which we ACK
// immediately (so Chrome keeps producing) and hand to onFrame. Responses to our
// own sends are drained and ignored.
func (c *cdpClient) readLoop() {
	defer close(c.done)
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var msg struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.Method != "Page.screencastFrame" {
			continue // response to a send, or an event we ignore
		}
		c.handleScreencastFrame(msg.Params)
	}
}

// handleScreencastFrame decodes one frame, ACKs it right away, records its
// device dimensions for input mapping, and forwards it to onFrame.
func (c *cdpClient) handleScreencastFrame(raw json.RawMessage) {
	var ev struct {
		Data      string `json:"data"`
		SessionID int    `json:"sessionId"`
		Metadata  struct {
			DeviceWidth  float64 `json:"deviceWidth"`
			DeviceHeight float64 `json:"deviceHeight"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return
	}
	// ACK first and unconditionally: CDP pauses the screencast after a couple of
	// un-acked frames, so acking immediately (BEFORE any viewer write) is what
	// keeps frames flowing. Backpressure is handled downstream by coalescing, not
	// by withholding acks. This ack is load-bearing; a test pins it.
	_ = c.send("Page.screencastFrameAck", map[string]any{"sessionId": ev.SessionID})

	if ev.Metadata.DeviceWidth > 0 && ev.Metadata.DeviceHeight > 0 {
		c.metaMu.Lock()
		c.lastW = ev.Metadata.DeviceWidth
		c.lastH = ev.Metadata.DeviceHeight
		c.metaMu.Unlock()
	}

	img, err := base64.StdEncoding.DecodeString(ev.Data)
	if err != nil || len(img) == 0 {
		return
	}
	if c.onFrame != nil {
		c.onFrame(screencastFrame{
			jpeg:         img,
			deviceWidth:  ev.Metadata.DeviceWidth,
			deviceHeight: ev.Metadata.DeviceHeight,
		})
	}
}

// Done returns a channel closed when the CDP read loop ends (the socket closed
// or the browser died). The viewer handler selects on it so a session that stops
// out from under a live viewer tears the viewer down promptly.
func (c *cdpClient) Done() <-chan struct{} { return c.done }

// startScreencast enables the Page domain and begins a JPEG screencast. everyNth
// throttles capture at the source (Chrome only emits every Nth frame), the first
// coarse backpressure lever before the coalescing holder.
func (c *cdpClient) startScreencast(quality, maxWidth, maxHeight, everyNth int) error {
	if err := c.send("Page.enable", nil); err != nil {
		return err
	}
	return c.send("Page.startScreencast", map[string]any{
		"format":        "jpeg",
		"quality":       quality,
		"maxWidth":      maxWidth,
		"maxHeight":     maxHeight,
		"everyNthFrame": everyNth,
	})
}

// stopScreencast asks Chrome to stop emitting frames. Best-effort politeness:
// Chrome also detaches when the CDP socket closes, so Close() is the real no-leak
// guarantee, not this call.
func (c *cdpClient) stopScreencast() { _ = c.send("Page.stopScreencast", nil) }

// dispatchMouse maps a viewer mouse event (normalized coords) to CDP viewport
// CSS pixels using the latest frame's device dimensions, then dispatches it.
func (c *cdpClient) dispatchMouse(m InputMessage) error {
	c.metaMu.Lock()
	w, h := c.lastW, c.lastH
	c.metaMu.Unlock()
	if w <= 0 || h <= 0 {
		// No frame seen yet: nothing sane to map against; drop the event rather
		// than dispatch at (0,0).
		return nil
	}
	params := map[string]any{
		"type":       m.Event,
		"x":          clamp01(m.X) * w,
		"y":          clamp01(m.Y) * h,
		"button":     mouseButton(m.Button),
		"buttons":    m.Buttons,
		"clickCount": m.ClickCount,
		"modifiers":  m.Modifiers,
	}
	if m.Event == "mouseWheel" {
		params["deltaX"] = m.DeltaX
		params["deltaY"] = m.DeltaY
	}
	return c.send("Input.dispatchMouseEvent", params)
}

// dispatchKey forwards a viewer key event straight to CDP.
func (c *cdpClient) dispatchKey(m InputMessage) error {
	params := map[string]any{
		"type":      m.KeyEvent,
		"modifiers": m.Modifiers,
	}
	if m.Key != "" {
		params["key"] = m.Key
	}
	if m.Code != "" {
		params["code"] = m.Code
	}
	if m.Text != "" {
		params["text"] = m.Text
	}
	if m.KeyCode != 0 {
		params["windowsVirtualKeyCode"] = m.KeyCode
	}
	return c.send("Input.dispatchKeyEvent", params)
}

// mouseButton defaults an empty/unknown button to "none" so a bare mouseMoved
// (no button) is valid CDP.
func mouseButton(b string) string {
	switch b {
	case "left", "middle", "right", "back", "forward":
		return b
	default:
		return "none"
	}
}

// Close tears the CDP socket down exactly once and waits for the read loop to
// drain. Closing the socket is what detaches CDP from the page (no leaked
// attachment), independent of any prior stopScreencast.
func (c *cdpClient) Close() {
	c.closeOnce.Do(func() {
		c.writeMu.Lock()
		_ = c.conn.Close()
		c.writeMu.Unlock()
	})
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
	}
}

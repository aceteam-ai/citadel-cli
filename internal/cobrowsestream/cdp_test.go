package cobrowsestream

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeChrome is a minimal CDP endpoint over a WebSocket: it emits screencast
// frames gated on acks (so a test can prove acking is what keeps frames flowing)
// and records the Input.dispatch* commands it receives.
type fakeChrome struct {
	server *httptest.Server

	totalFrames  int     // frames to emit across the session
	deviceWidth  float64 // advertised frame width
	deviceHeight float64 // advertised frame height
	jpeg         []byte  // raw bytes each frame carries (base64 on the wire)

	mu      sync.Mutex
	acks    int
	mouse   []map[string]any
	keys    []map[string]any
	conns   []*websocket.Conn
	started bool

	// connReg is signaled once per accepted CDP conn, AFTER it has been appended
	// to conns. dialCDP returns to the test the instant the client sees the 101
	// upgrade response, which can be strictly before this handler goroutine runs
	// the append -- so a test that wants to act on a registered conn (e.g.
	// killConns) must wait on this first, or it races an empty conns slice.
	connReg chan struct{}
}

func newFakeChrome() *fakeChrome {
	f := &fakeChrome{
		totalFrames:  3,
		deviceWidth:  800,
		deviceHeight: 600,
		jpeg:         []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x01, 0x02, 0x03}, // JPEG-ish
		connReg:      make(chan struct{}, 16),
	}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/devtools/page/1", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns = append(f.conns, conn)
		f.mu.Unlock()
		// Registration is now visible to conns; signal any waiter (best-effort,
		// buffered) before we block reading so killConns can't race the append.
		select {
		case f.connReg <- struct{}{}:
		default:
		}
		f.serve(conn)
	})
	f.server = httptest.NewServer(mux)
	return f
}

// wsURL returns the DevTools WebSocket URL cdpTargetURL should resolve to.
func (f *fakeChrome) wsURL() string {
	return "ws" + strings.TrimPrefix(f.server.URL, "http") + "/devtools/page/1"
}

func (f *fakeChrome) serve(conn *websocket.Conn) {
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var msg struct {
			ID     int            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg.Method {
		case "Page.startScreencast":
			f.mu.Lock()
			f.started = true
			f.mu.Unlock()
			f.sendFrame(conn, 1)
		case "Page.screencastFrameAck":
			f.mu.Lock()
			f.acks++
			n := f.acks
			total := f.totalFrames
			f.mu.Unlock()
			if n < total {
				f.sendFrame(conn, n+1)
			}
		case "Input.dispatchMouseEvent":
			f.mu.Lock()
			f.mouse = append(f.mouse, msg.Params)
			f.mu.Unlock()
		case "Input.dispatchKeyEvent":
			f.mu.Lock()
			f.keys = append(f.keys, msg.Params)
			f.mu.Unlock()
		case "Runtime.evaluate":
			_ = conn.WriteJSON(map[string]any{
				"id":     msg.ID,
				"result": map[string]any{"result": map[string]any{"value": 12.5}},
			})
		}
	}
}

func TestExportedCDPClientWaitsForMatchingResponse(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	withFakeCDP(t, f)

	c, err := DialCDP(9222)
	if err != nil {
		t.Fatalf("DialCDP: %v", err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := c.Command(ctx, "Runtime.evaluate", map[string]any{"expression": "window.DUR"})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	inner, _ := res["result"].(map[string]any)
	if got, _ := inner["value"].(float64); got != 12.5 {
		t.Fatalf("value = %v, want 12.5 (full response %v)", got, res)
	}
}

func (f *fakeChrome) sendFrame(conn *websocket.Conn, seq int) {
	ev := map[string]any{
		"method": "Page.screencastFrame",
		"params": map[string]any{
			"data":      base64.StdEncoding.EncodeToString(f.jpeg),
			"sessionId": seq,
			"metadata": map[string]any{
				"deviceWidth":  f.deviceWidth,
				"deviceHeight": f.deviceHeight,
			},
		},
	}
	_ = conn.WriteJSON(ev)
}

// waitConnRegistered blocks until the server handler has accepted and appended
// at least one CDP conn (see connReg). Callers must do this before killConns:
// dialCDP returns as soon as the client reads the 101 upgrade response, which
// can precede the handler's append, so killing "now" may find conns empty and
// close nothing -- leaving both read loops blocked forever (citadel #819).
func (f *fakeChrome) waitConnRegistered(t *testing.T) {
	t.Helper()
	select {
	case <-f.connReg:
	case <-time.After(10 * time.Second):
		t.Fatal("server never registered a CDP conn")
	}
}

// killConns closes the server-side CDP sockets, simulating the browser going
// away (session stop) under a live viewer. A caller must first establish that
// the conn has been registered (appended to conns) -- either via
// waitConnRegistered, or by reading a frame that proves f.serve is already
// running (as TestTeardownOnBrowserStop does). Otherwise this can run before the
// handler's append and close nothing, leaving both read loops blocked forever.
func (f *fakeChrome) killConns() {
	f.mu.Lock()
	conns := append([]*websocket.Conn(nil), f.conns...)
	f.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (f *fakeChrome) close() { f.server.Close() }

func (f *fakeChrome) mouseEvents() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.mouse...)
}

// withFakeCDP points cdpTargetURL at the fake for the duration of a test.
func withFakeCDP(t *testing.T, f *fakeChrome) {
	t.Helper()
	orig := cdpTargetURL
	cdpTargetURL = func(int) (string, error) { return f.wsURL(), nil }
	t.Cleanup(func() { cdpTargetURL = orig })
}

// TestCDPScreencastAckKeepsFramesFlowing pins the load-bearing ack: the fake
// gates each subsequent frame on receiving an ack for the previous one, so if
// handleScreencastFrame stopped acking, only the first frame would arrive.
func TestCDPScreencastAckKeepsFramesFlowing(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	withFakeCDP(t, f)

	frames := make(chan screencastFrame, 16)
	cdp, err := dialCDP(9222, func(fr screencastFrame) { frames <- fr })
	if err != nil {
		t.Fatalf("dialCDP: %v", err)
	}
	defer cdp.Close()

	if err := cdp.startScreencast(60, 1280, 720, 1); err != nil {
		t.Fatalf("startScreencast: %v", err)
	}

	// Expect exactly totalFrames frames, each decoded back to the raw JPEG bytes.
	for i := 0; i < f.totalFrames; i++ {
		select {
		case fr := <-frames:
			if string(fr.jpeg) != string(f.jpeg) {
				t.Fatalf("frame %d jpeg mismatch: got %v", i, fr.jpeg)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for frame %d (ack not driving frames?)", i)
		}
	}
}

// TestCDPMouseCoordinateMapping verifies normalized [0,1] viewer coords are
// mapped to CDP viewport pixels using the latest frame's device dimensions.
func TestCDPMouseCoordinateMapping(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	withFakeCDP(t, f)

	frames := make(chan screencastFrame, 16)
	cdp, err := dialCDP(9222, func(fr screencastFrame) { frames <- fr })
	if err != nil {
		t.Fatalf("dialCDP: %v", err)
	}
	defer cdp.Close()
	if err := cdp.startScreencast(60, 1280, 720, 1); err != nil {
		t.Fatalf("startScreencast: %v", err)
	}
	// Wait for one frame so lastW/lastH are populated (800x600).
	select {
	case <-frames:
	case <-time.After(3 * time.Second):
		t.Fatal("no frame")
	}

	if err := cdp.dispatchMouse(InputMessage{
		Type: InputTypeMouse, Event: "mousePressed", X: 0.5, Y: 0.25, Button: "left", ClickCount: 1,
	}); err != nil {
		t.Fatalf("dispatchMouse: %v", err)
	}

	// Poll for the recorded event (dispatch is fire-and-forget over the wire).
	deadline := time.Now().Add(3 * time.Second)
	for {
		ev := f.mouseEvents()
		if len(ev) > 0 {
			x, _ := ev[0]["x"].(float64)
			y, _ := ev[0]["y"].(float64)
			if x != 400 { // 0.5 * 800
				t.Errorf("x = %v, want 400", x)
			}
			if y != 150 { // 0.25 * 600
				t.Errorf("y = %v, want 150", y)
			}
			if ev[0]["button"] != "left" {
				t.Errorf("button = %v, want left", ev[0]["button"])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("mouse event never recorded")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestCDPCloseSignalsDone verifies the browser going away closes Done() so the
// viewer handler can tear down promptly.
func TestCDPCloseSignalsDone(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	withFakeCDP(t, f)

	cdp, err := dialCDP(9222, func(screencastFrame) {})
	if err != nil {
		t.Fatalf("dialCDP: %v", err)
	}
	defer cdp.Close()

	// Wait until the fake has actually registered the conn before killing it.
	// Without this, killConns can run before the handler appends the conn (dialCDP
	// returns on the 101 upgrade, which precedes the append under parallel load),
	// close nothing, and leave the read loop blocked forever -- the #819 flake,
	// which failed a release. The 3s wall-clock guard the select used before was a
	// red herring: the real defect was this unsynchronized close, not a tight
	// deadline. Once the kill is deterministic, Done() closes in ms.
	f.waitConnRegistered(t)
	f.killConns()
	select {
	case <-cdp.Done():
	case <-time.After(10 * time.Second):
		// Safety net only: with the kill synchronized above this never fires
		// unless Done() genuinely regresses.
		t.Fatal("Done() not closed after browser socket dropped")
	}
}

// TestCDPMouseDropsWithoutFrame verifies input is dropped (not dispatched at
// 0,0) before any frame establishes the coordinate scale.
func TestCDPMouseDropsWithoutFrame(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	withFakeCDP(t, f)

	cdp, err := dialCDP(9222, func(screencastFrame) {})
	if err != nil {
		t.Fatalf("dialCDP: %v", err)
	}
	defer cdp.Close()

	if err := cdp.dispatchMouse(InputMessage{Type: InputTypeMouse, Event: "mouseMoved", X: 0.5, Y: 0.5}); err != nil {
		t.Fatalf("dispatchMouse: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := len(f.mouseEvents()); got != 0 {
		t.Errorf("dispatched %d mouse events before any frame, want 0", got)
	}
}

// The server deliberately keeps the upgraded socket open after sending. A
// client Done signal must therefore come from the read refusal, not server EOF.
func withCDPMessageFixture(t *testing.T, send func(*websocket.Conn) error) (<-chan error, <-chan http.Header) {
	t.Helper()
	release := make(chan struct{})
	sent := make(chan error, 1)
	headers := make(chan http.Header, 1)
	up := websocket.Upgrader{WriteBufferSize: 64, EnableCompression: true}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Clone()
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			sent <- err
			return
		}
		defer conn.Close()
		sent <- send(conn)
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	original := cdpTargetURL
	cdpTargetURL = func(int) (string, error) {
		return "ws" + strings.TrimPrefix(server.URL, "http"), nil
	}
	t.Cleanup(func() { cdpTargetURL = original })
	return sent, headers
}

func requireCDPMessageSent(t *testing.T, sent <-chan error) {
	t.Helper()
	select {
	case err := <-sent:
		if err != nil {
			t.Fatalf("fake CDP write: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fake CDP never sent its message")
	}
}

func TestCDPReadLimitRejectsImmediateFirstMessageAndFragmentAggregate(t *testing.T) {
	const limit int64 = 128
	for _, fragmented := range []bool{false, true} {
		name := "first_message"
		if fragmented {
			name = "fragment_aggregate"
		}
		t.Run(name, func(t *testing.T) {
			payload := []byte(`{"method":"fixture","params":{"pad":"` + strings.Repeat("x", 256) + `"}}`)
			sent, _ := withCDPMessageFixture(t, func(conn *websocket.Conn) error {
				if !fragmented {
					return conn.WriteMessage(websocket.TextMessage, payload)
				}
				writer, err := conn.NextWriter(websocket.TextMessage)
				if err != nil {
					return err
				}
				// Each write is smaller than the 64-byte server frame buffer,
				// which flushes continuation frames below the 128-byte limit.
				for start := 0; start < len(payload); start += 16 {
					end := start + 16
					if end > len(payload) {
						end = len(payload)
					}
					if _, err := writer.Write(payload[start:end]); err != nil {
						return err
					}
				}
				return writer.Close()
			})
			client, err := dialCDPWithReadLimit(9222, nil, limit)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			t.Cleanup(client.Close)
			requireCDPMessageSent(t, sent)
			select {
			case <-client.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("oversized first message did not end the reader while server stayed open")
			}
		})
	}
}

func TestCDPReadLimitAcceptsExactAndBelowLimitMessages(t *testing.T) {
	const limit int64 = 128
	for _, test := range []struct {
		name string
		size int
	}{{"below", 127}, {"exact", 128}} {
		t.Run(test.name, func(t *testing.T) {
			prefix, suffix := `{"method":"fixture","params":{"pad":"`, `"}}`
			payload := []byte(prefix + strings.Repeat("x", test.size-len(prefix)-len(suffix)) + suffix)
			sent, _ := withCDPMessageFixture(t, func(conn *websocket.Conn) error {
				if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
					return err
				}
				var request struct {
					ID int `json:"id"`
				}
				if err := conn.ReadJSON(&request); err != nil {
					return err
				}
				return conn.WriteJSON(map[string]any{"id": request.ID, "result": map[string]any{"ok": true}})
			})
			client, err := dialCDPWithReadLimit(9222, nil, limit)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			t.Cleanup(client.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result, err := client.command(ctx, "Runtime.evaluate", nil)
			if err != nil || result["ok"] != true {
				t.Fatalf("accepted message interrupted command: %#v %v", result, err)
			}
			requireCDPMessageSent(t, sent)
		})
	}
}

func TestCDPReadLimitFailsPendingCommandWithoutContextTimeout(t *testing.T) {
	sent, _ := withCDPMessageFixture(t, func(conn *websocket.Conn) error {
		var request struct {
			ID int `json:"id"`
		}
		if err := conn.ReadJSON(&request); err != nil {
			return err
		}
		return conn.WriteJSON(map[string]any{"id": request.ID, "result": map[string]any{"data": strings.Repeat("x", 256)}})
	})
	client, err := dialCDPWithReadLimit(9222, nil, 128)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(client.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = client.command(ctx, "Page.captureScreenshot", nil)
	if err == nil || !strings.Contains(err.Error(), "connection closed") || ctx.Err() != nil {
		t.Fatalf("pending command did not fail on read refusal: %v (context %v)", err, ctx.Err())
	}
	requireCDPMessageSent(t, sent)
}

func TestCDPDialPinsCompressionOffWithoutMutatingDefault(t *testing.T) {
	original := websocket.DefaultDialer
	configured := *original
	configured.EnableCompression = true
	websocket.DefaultDialer = &configured
	t.Cleanup(func() { websocket.DefaultDialer = original })
	sent, headers := withCDPMessageFixture(t, func(*websocket.Conn) error { return nil })
	client, err := dialCDPWithReadLimit(9222, nil, 128)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(client.Close)
	requireCDPMessageSent(t, sent)
	select {
	case header := <-headers:
		if got := header.Get("Sec-WebSocket-Extensions"); got != "" {
			t.Fatalf("CDP offered compression: %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fake CDP did not record upgrade headers")
	}
	if websocket.DefaultDialer != &configured || !configured.EnableCompression {
		t.Fatal("CDP dial mutated the shared DefaultDialer")
	}
}

func TestCDPSmallLimitSeamCannotDisableOrIncreaseBudget(t *testing.T) {
	original := cdpTargetURL
	lookups := 0
	cdpTargetURL = func(int) (string, error) {
		lookups++
		return "", nil
	}
	t.Cleanup(func() { cdpTargetURL = original })
	for _, limit := range []int64{-1, 0, maxCDPMessageBytes + 1} {
		client, err := dialCDPWithReadLimit(9222, nil, limit)
		if client != nil || err == nil {
			t.Fatalf("invalid limit %d accepted: %v", limit, err)
		}
	}
	if lookups != 0 {
		t.Fatal("invalid read limit reached target discovery")
	}
}

type rawCDPReport struct {
	writeErr      error
	readErr       error
	receivedBytes int
}

func rawCDPFrame(w io.Writer, payload []byte, compressed bool) error {
	first := byte(0x81) // final text frame, unmasked server payload
	if compressed {
		first |= 0x40 // RSV1 for negotiated permessage-deflate
	}
	if len(payload) > 65535 {
		return fmt.Errorf("raw fixture payload is too large")
	}
	header := []byte{first, byte(len(payload))}
	if len(payload) >= 126 {
		header = []byte{first, 126, byte(len(payload) >> 8), byte(len(payload))}
	}
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func deflateCDPFixture(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	if len(data) < 4 || !bytes.Equal(data[len(data)-4:], []byte{0, 0, 255, 255}) {
		t.Fatal("fixture lacks permessage-deflate sync-flush trailer")
	}
	frame := append([]byte(nil), data[:len(data)-4]...)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return frame
}

// Raw upgrade responses intentionally bypass Upgrader's extension negotiation.
// The SHA1 accept and frame writer are shared with the positive counterpart.
// After observing transport EOF/error, the handler still waits for release, so
// server teardown cannot manufacture the client's refusal/close observation.
func withRawCDPUpgrade(t *testing.T, extensions []string, compressed bool) <-chan rawCDPReport {
	t.Helper()
	payload := []byte(`{"method":"Page.screencastFrame","params":{"data":"eA==","sessionId":1,"metadata":{"deviceWidth":1,"deviceHeight":1},"padding":"` + strings.Repeat("x", 32<<10) + `"}}`)
	if compressed {
		payload = deflateCDPFixture(t, payload)
		if len(payload) >= 512 {
			t.Fatal("compressed fixture must fit below the 512-byte wire limit")
		}
	} else if len(extensions) != 0 {
		// Keep all rejecting handshakes plus first frame in one small flush;
		// their required immediate client close must not race a large write.
		payload = []byte(`{"method":"Page.screencastFrame","params":{"data":"eA==","sessionId":1,"metadata":{"deviceWidth":1,"deviceHeight":1}}}`)
	}
	release := make(chan struct{})
	reports := make(chan rawCDPReport, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			reports <- rawCDPReport{writeErr: fmt.Errorf("fixture cannot hijack HTTP connection")}
			return
		}
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			reports <- rawCDPReport{writeErr: err}
			return
		}
		defer conn.Close()
		accept := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		var response bytes.Buffer
		_, _ = fmt.Fprintf(&response, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n", base64.StdEncoding.EncodeToString(accept[:]))
		for _, extension := range extensions {
			_, _ = fmt.Fprintf(&response, "Sec-WebSocket-Extensions: %s\r\n", extension)
		}
		response.WriteString("\r\n")
		writeErr := rawCDPFrame(&response, payload, compressed)
		if writeErr == nil {
			_, writeErr = rw.Write(response.Bytes())
		}
		if writeErr == nil {
			writeErr = rw.Flush()
		}
		report := rawCDPReport{writeErr: writeErr}
		if writeErr == nil {
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			var buf [256]byte
			for {
				n, readErr := rw.Read(buf[:])
				report.receivedBytes += n
				if readErr != nil {
					report.readErr = readErr
					break
				}
			}
		}
		reports <- report
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	original := cdpTargetURL
	cdpTargetURL = func(int) (string, error) {
		return "ws" + strings.TrimPrefix(server.URL, "http"), nil
	}
	t.Cleanup(func() { cdpTargetURL = original })
	return reports
}

func requireRawCDPTransportClosed(t *testing.T, reports <-chan rawCDPReport) rawCDPReport {
	t.Helper()
	select {
	case report := <-reports:
		if report.writeErr != nil || report.readErr == nil {
			t.Fatalf("raw fixture did not observe client transport close: %#v", report)
		}
		if timeout, ok := report.readErr.(net.Error); ok && timeout.Timeout() {
			t.Fatal("raw fixture timed out instead of observing client transport close")
		}
		return report
	case <-time.After(5 * time.Second):
		t.Fatal("raw fixture never observed client transport close")
		return rawCDPReport{}
	}
}

func TestCDPDialRejectsUnsolicitedAndDuplicateExtensionResponses(t *testing.T) {
	const deflate = "permessage-deflate; server_no_context_takeover; client_no_context_takeover"
	for _, test := range []struct {
		name       string
		extensions []string
		compressed bool
	}{
		{"unsolicited_deflate", []string{deflate}, true},
		{"empty_then_deflate", []string{"", deflate}, true},
		{"empty_extension", []string{""}, false},
		{"unknown_extension", []string{"fixture-extension"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			reports := withRawCDPUpgrade(t, test.extensions, test.compressed)
			frames := make(chan screencastFrame, 1)
			client, err := dialCDPWithReadLimit(9222, func(frame screencastFrame) { frames <- frame }, 512)
			if client != nil {
				t.Cleanup(client.Close)
			}
			if client != nil || err == nil || err.Error() != "CDP extensions are not allowed" {
				t.Fatalf("valid raw upgrade was not refused at application extension boundary: %v %v", client, err)
			}
			report := requireRawCDPTransportClosed(t, reports)
			if report.receivedBytes != 0 {
				t.Fatalf("rejected peer reached reader/ACK path: %d client bytes", report.receivedBytes)
			}
			select {
			case <-frames:
				t.Fatal("rejected peer reached frame callback")
			default:
			}
		})
	}
}

func TestCDPRawUncompressedUpgradePositiveCounterpart(t *testing.T) {
	reports := withRawCDPUpgrade(t, nil, false)
	frames := make(chan screencastFrame, 1)
	client, err := dialCDPWithReadLimit(9222, func(frame screencastFrame) { frames <- frame }, 64<<10)
	if err != nil {
		t.Fatalf("raw valid uncompressed upgrade failed: %v", err)
	}
	t.Cleanup(client.Close)
	select {
	case frame := <-frames:
		if string(frame.jpeg) != "x" {
			t.Fatalf("raw fixture frame = %q", frame.jpeg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("raw positive fixture never reached frame callback")
	}
	client.Close()
	report := requireRawCDPTransportClosed(t, reports)
	if report.receivedBytes == 0 {
		t.Fatal("raw positive fixture never observed the screencast ACK")
	}
}

package desktop

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// VNCServer is a minimal RFB 3.8 server that captures the local screen and
// serves it over the VNC protocol (Raw encoding, No authentication).
//
// It listens on localhost and optionally on additional listeners (e.g. tsnet
// VPN) added via AddListener, mirroring the terminal server pattern.
type VNCServer struct {
	host string
	port int
	fps  int

	capturer Capturer
	logger   Logger

	mu             sync.RWMutex
	running        bool
	listener       net.Listener
	extraListeners []net.Listener
	stopCh         chan struct{}

	activeConns int64
	totalConns  int64
}

// VNCServerConfig holds configuration for the VNC server.
type VNCServerConfig struct {
	Host string // Bind host (default "127.0.0.1")
	Port int    // Bind port (default 5900)
	FPS  int    // Target frame rate (default 10)
}

// Logger is the interface for VNC server logging, matching the terminal server.
type Logger interface {
	Printf(format string, v ...interface{})
}

type stdLogger struct{ l *log.Logger }

func (s *stdLogger) Printf(format string, v ...interface{}) { s.l.Printf(format, v...) }

type noOpLogger struct{}

func (n *noOpLogger) Printf(format string, v ...interface{}) {}

// NewVNCServer creates a new VNC server with the given configuration.
func NewVNCServer(cfg VNCServerConfig) *VNCServer {
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.Port == 0 {
		cfg.Port = 5900
	}
	if cfg.FPS <= 0 || cfg.FPS > 60 {
		cfg.FPS = 10
	}

	return &VNCServer{
		host:     cfg.Host,
		port:     cfg.Port,
		fps:      cfg.FPS,
		capturer: newCapturer(),
		logger:   &stdLogger{l: log.New(os.Stderr, "[vnc] ", log.LstdFlags)},
		stopCh:   make(chan struct{}),
	}
}

// SetSilent switches to a no-op logger (for TUI mode).
func (s *VNCServer) SetSilent() { s.logger = &noOpLogger{} }

// AddListener registers an additional net.Listener (e.g. VPN) that the server
// will accept connections on.
//
// If the server is already running, the listener begins accepting connections
// immediately. This lets callers re-attach a VPN listener after a tsnet
// reconnect without restarting the server (see issue #317). If the server is
// not yet running, the listener is queued and served when Start is called.
func (s *VNCServer) AddListener(ln net.Listener) {
	s.mu.Lock()
	s.extraListeners = append(s.extraListeners, ln)
	running := s.running
	s.mu.Unlock()

	if running {
		s.logger.Printf("VNC server also listening on %s (VPN, hot-attached)", ln.Addr())
		go s.acceptLoop(ln)
	}
}

// RemoveListener drops a previously added extra listener from the server's
// tracking slice so a long-lived session that re-attaches a VPN listener across
// many reconnects does not accumulate dead listener references (issue #317).
// It does not close the listener; the caller owns its lifecycle.
func (s *VNCServer) RemoveListener(ln net.Listener) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, l := range s.extraListeners {
		if l == ln {
			s.extraListeners = append(s.extraListeners[:i], s.extraListeners[i+1:]...)
			return
		}
	}
}

// Start initializes the screen capturer and begins accepting VNC connections.
func (s *VNCServer) Start() error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("VNC server already running")
	}
	s.running = true
	s.mu.Unlock()

	// Initialize screen capture
	if err := s.capturer.Init(); err != nil {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		return fmt.Errorf("screen capture init: %w", err)
	}

	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.capturer.Close()
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	s.listener = ln

	s.logger.Printf("VNC server listening on %s (%d FPS)", ln.Addr(), s.fps)

	go s.acceptLoop(ln)

	for _, extra := range s.extraListeners {
		extra := extra
		s.logger.Printf("VNC server also listening on %s (VPN)", extra.Addr())
		go s.acceptLoop(extra)
	}

	return nil
}

// Stop gracefully shuts down the VNC server.
func (s *VNCServer) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	s.mu.Unlock()

	close(s.stopCh)

	if s.listener != nil {
		s.listener.Close()
	}
	for _, ln := range s.extraListeners {
		ln.Close()
	}
	s.capturer.Close()
	s.logger.Printf("VNC server stopped (total=%d)", atomic.LoadInt64(&s.totalConns))
}

// IsRunning returns whether the server is currently running.
func (s *VNCServer) IsRunning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.running
}

// Port returns the configured port.
func (s *VNCServer) Port() int { return s.port }

// ActiveConnections returns the number of active VNC sessions.
func (s *VNCServer) ActiveConnections() int64 {
	return atomic.LoadInt64(&s.activeConns)
}

func (s *VNCServer) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.stopCh:
				return
			default:
			}
			// A closed listener (e.g. a tsnet VPN listener torn down on
			// reconnect) returns a permanent error. Exiting the loop avoids a
			// busy-spin and lets the supervisor re-attach a fresh listener
			// (issue #317). Transient errors are retried.
			if errors.Is(err, net.ErrClosed) {
				s.logger.Printf("listener closed (%s), stopping accept loop", ln.Addr())
				return
			}
			s.logger.Printf("accept error: %v", err)
			continue
		}
		atomic.AddInt64(&s.totalConns, 1)
		go s.handleConn(conn)
	}
}

// handleConn manages the full RFB lifecycle for a single client connection.
func (s *VNCServer) handleConn(conn net.Conn) {
	atomic.AddInt64(&s.activeConns, 1)
	defer func() {
		conn.Close()
		atomic.AddInt64(&s.activeConns, -1)
	}()

	remote := conn.RemoteAddr().String()
	s.logger.Printf("client connected: %s", remote)
	defer s.logger.Printf("client disconnected: %s", remote)

	if err := s.rfbHandshake(conn); err != nil {
		s.logger.Printf("handshake failed (%s): %v", remote, err)
		return
	}

	s.rfbSession(conn)
}

// rfbHandshake performs the RFB 3.8 protocol handshake (version, security, init).
func (s *VNCServer) rfbHandshake(conn net.Conn) error {
	// 1. Server sends protocol version
	if _, err := conn.Write([]byte("RFB 003.008\n")); err != nil {
		return fmt.Errorf("write version: %w", err)
	}

	// 2. Client responds with version
	var clientVersion [12]byte
	if _, err := io.ReadFull(conn, clientVersion[:]); err != nil {
		return fmt.Errorf("read client version: %w", err)
	}

	// 3. Server sends security types: 1 type, type=1 (None)
	if _, err := conn.Write([]byte{1, 1}); err != nil {
		return fmt.Errorf("write security types: %w", err)
	}

	// 4. Client selects security type
	var secType [1]byte
	if _, err := io.ReadFull(conn, secType[:]); err != nil {
		return fmt.Errorf("read security type: %w", err)
	}
	if secType[0] != 1 {
		return fmt.Errorf("client selected unsupported security type %d", secType[0])
	}

	// 5. RFB 3.8: send SecurityResult (u32 0 = OK) for None auth
	if err := binary.Write(conn, binary.BigEndian, uint32(0)); err != nil {
		return fmt.Errorf("write security result: %w", err)
	}

	// 6. Client sends ClientInit (shared-flag byte)
	var clientInit [1]byte
	if _, err := io.ReadFull(conn, clientInit[:]); err != nil {
		return fmt.Errorf("read ClientInit: %w", err)
	}

	// 7. Server sends ServerInit
	frame, err := s.capturer.Capture()
	if err != nil {
		return fmt.Errorf("initial capture: %w", err)
	}
	if err := s.writeServerInit(conn, frame.Width(), frame.Height()); err != nil {
		return fmt.Errorf("write ServerInit: %w", err)
	}

	return nil
}

// ServerInit pixel format: 32bpp RGBX, matching what noVNC requests.
// GDI captures in BGRX, we swap B↔R before sending (see writeFramebufferUpdate).
// bpp=32, depth=24, big-endian=0, true-color=1
// red-max=255, green-max=255, blue-max=255
// red-shift=0, green-shift=8, blue-shift=16
var serverPixelFormat = [16]byte{
	32,   // bits-per-pixel
	24,   // depth
	0,    // big-endian-flag (little-endian)
	1,    // true-colour-flag
	0, 255, // red-max (big-endian u16)
	0, 255, // green-max
	0, 255, // blue-max
	0,   // red-shift
	8,   // green-shift
	16,  // blue-shift
	0, 0, 0, // padding (3 bytes)
}

// writeServerInit writes the ServerInit message.
func (s *VNCServer) writeServerInit(w io.Writer, width, height int) error {
	// framebuffer-width (u16) + framebuffer-height (u16)
	if err := binary.Write(w, binary.BigEndian, uint16(width)); err != nil {
		return err
	}
	if err := binary.Write(w, binary.BigEndian, uint16(height)); err != nil {
		return err
	}

	// server-pixel-format (16 bytes)
	if _, err := w.Write(serverPixelFormat[:]); err != nil {
		return err
	}

	// name-length (u32) + name-string
	name := []byte("Citadel Desktop")
	if err := binary.Write(w, binary.BigEndian, uint32(len(name))); err != nil {
		return err
	}
	if _, err := w.Write(name); err != nil {
		return err
	}

	return nil
}

// rfbSession runs the main client message loop and frame-sending goroutine.
func (s *VNCServer) rfbSession(conn net.Conn) {
	var writeMu sync.Mutex
	// updateRequested carries the incremental flag of a FramebufferUpdateRequest
	// (true = incremental, false = full). Buffered to 1; a pending full request
	// wins over a later incremental one.
	updateRequested := make(chan bool, 1)
	done := make(chan struct{})

	// Per-connection encoding negotiation state. zrleEnabled is set when the
	// client advertises ZRLE (16) via SetEncodings. Protected by encMu because
	// it is written by the reader loop and read by the sender goroutine.
	var encMu sync.Mutex
	zrleEnabled := false

	// Frame sender goroutine
	go func() {
		defer close(done)
		interval := time.Duration(float64(time.Second) / float64(s.fps))
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// Per-connection state: the previous captured frame (native BGRX) for
		// dirty-rectangle diffing, and the continuous ZRLE zlib stream.
		var prevFrame []byte
		var zEnc *zrleEncoder
		defer func() {
			if zEnc != nil {
				zEnc.Close()
			}
		}()

		for {
			select {
			case <-s.stopCh:
				return
			case incremental := <-updateRequested:
				frame, err := s.capturer.Capture()
				if err != nil {
					s.logger.Printf("capture error: %v", err)
					continue
				}

				encMu.Lock()
				useZRLE := zrleEnabled
				encMu.Unlock()

				if useZRLE && zEnc == nil {
					zEnc = newZRLEEncoder()
				}

				// Determine the rectangles to send. A non-incremental request,
				// or the first frame of a connection, sends the full frame.
				// Otherwise diff against the previous frame on a 64x64 tile grid.
				frameW, frameH := frame.Width(), frame.Height()
				var rects []image.Rectangle
				if !incremental || prevFrame == nil {
					rects = []image.Rectangle{image.Rect(0, 0, frameW, frameH)}
				} else {
					rects = dirtyTiles(prevFrame, frame.Pix, frameW, frameH)
				}

				activeEnc := zEnc
				if !useZRLE {
					activeEnc = nil
				}

				writeMu.Lock()
				err = writeFramebufferUpdateRects(conn, frame.Pix, frameW, rects, activeEnc)
				writeMu.Unlock()
				if err != nil {
					return // connection dead
				}

				// Stash a copy of this frame as the diff baseline. Capture
				// returns a fresh buffer each call, so we can retain it.
				prevFrame = frame.Pix
			}
		}
	}()

	// Client message reader loop
	for {
		select {
		case <-done:
			return
		case <-s.stopCh:
			return
		default:
		}

		// Read message type (1 byte)
		var msgType [1]byte
		conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		if _, err := io.ReadFull(conn, msgType[:]); err != nil {
			return
		}

		switch msgType[0] {
		case 0: // SetPixelFormat
			// type(1) + padding(3) + pixel-format(16) = 20 total, already read 1
			var buf [19]byte
			if _, err := io.ReadFull(conn, buf[:]); err != nil {
				return
			}
			// We ignore the client's requested pixel format and always send our native format.
			// Most clients (noVNC included) handle this gracefully.

		case 2: // SetEncodings
			// type(1) + padding(1) + number-of-encodings(u16)
			var header [3]byte // padding(1) + count(2)
			if _, err := io.ReadFull(conn, header[:]); err != nil {
				return
			}
			count := binary.BigEndian.Uint16(header[1:3])
			// Each encoding is s32 (4 bytes)
			encodings := make([]byte, int(count)*4)
			if _, err := io.ReadFull(conn, encodings); err != nil {
				return
			}
			// Negotiate ZRLE (16) if the client advertises it; otherwise we
			// fall back to Raw (0), which every client supports.
			if clientSupportsZRLE(encodings) {
				encMu.Lock()
				zrleEnabled = true
				encMu.Unlock()
				s.logger.Printf("client negotiated ZRLE encoding")
			}

		case 3: // FramebufferUpdateRequest
			// type(1) + incremental(1) + x(2) + y(2) + w(2) + h(2) = 10, already read 1
			var buf [9]byte
			if _, err := io.ReadFull(conn, buf[:]); err != nil {
				return
			}
			incremental := buf[0] != 0
			// Signal the frame sender that the client wants a frame, passing
			// the incremental flag. A pending full request (false) is not
			// downgraded by a later incremental one.
			select {
			case updateRequested <- incremental:
			default:
				if !incremental {
					// Ensure a full-update request is not lost behind a queued
					// incremental one: drain and re-send as full.
					select {
					case <-updateRequested:
					default:
					}
					select {
					case updateRequested <- false:
					default:
					}
				}
			}

		case 4: // KeyEvent
			// type(1) + down-flag(1) + padding(2) + key(4) = 8, already read 1
			var buf [7]byte
			if _, err := io.ReadFull(conn, buf[:]); err != nil {
				return
			}
			down := buf[0] != 0
			keysym := binary.BigEndian.Uint32(buf[3:7])
			sendKeyEvent(keysym, down)

		case 5: // PointerEvent
			// type(1) + button-mask(1) + x(2) + y(2) = 6, already read 1
			var buf [5]byte
			if _, err := io.ReadFull(conn, buf[:]); err != nil {
				return
			}
			buttonMask := buf[0]
			px := int(binary.BigEndian.Uint16(buf[1:3]))
			py := int(binary.BigEndian.Uint16(buf[3:5]))
			sendPointerEvent(px, py, buttonMask)

		case 6: // ClientCutText
			// type(1) + padding(3) + length(4) + text(length)
			var header [7]byte // padding(3) + length(4)
			if _, err := io.ReadFull(conn, header[:]); err != nil {
				return
			}
			textLen := binary.BigEndian.Uint32(header[3:7])
			if textLen > 0 {
				text := make([]byte, textLen)
				if _, err := io.ReadFull(conn, text); err != nil {
					return
				}
			}

		default:
			// Unknown message type — the stream is desynced, bail out.
			s.logger.Printf("unknown client message type %d, closing connection", msgType[0])
			return
		}
	}
}

// writeFramebufferUpdate sends a single full-screen FramebufferUpdate with Raw encoding.
func writeFramebufferUpdate(w io.Writer, frame *CaptureResult) error {
	width := frame.Width()
	height := frame.Height()

	// FramebufferUpdate header: type(0) + padding(1) + number-of-rects(u16)
	header := [4]byte{0, 0, 0, 1} // type=0, pad=0, nrects=1
	if _, err := w.Write(header[:]); err != nil {
		return err
	}

	// Rectangle header: x(u16) + y(u16) + width(u16) + height(u16) + encoding(s32)
	var rectHeader [12]byte
	binary.BigEndian.PutUint16(rectHeader[0:2], 0)              // x
	binary.BigEndian.PutUint16(rectHeader[2:4], 0)              // y
	binary.BigEndian.PutUint16(rectHeader[4:6], uint16(width))  // width
	binary.BigEndian.PutUint16(rectHeader[6:8], uint16(height)) // height
	binary.BigEndian.PutUint32(rectHeader[8:12], 0)             // encoding = Raw (0)
	if _, err := w.Write(rectHeader[:]); err != nil {
		return err
	}

	// Convert BGRX (GDI native) to RGBX (our advertised format) by swapping B↔R.
	pix := frame.Pix
	for i := 0; i < len(pix)-2; i += 4 {
		pix[i], pix[i+2] = pix[i+2], pix[i]
	}
	if _, err := w.Write(pix); err != nil {
		return err
	}

	return nil
}

// MarshalServerInit builds the ServerInit message bytes for testing.
func MarshalServerInit(width, height int, name string) []byte {
	nameBytes := []byte(name)
	buf := make([]byte, 2+2+16+4+len(nameBytes))

	binary.BigEndian.PutUint16(buf[0:2], uint16(width))
	binary.BigEndian.PutUint16(buf[2:4], uint16(height))
	copy(buf[4:20], serverPixelFormat[:])
	binary.BigEndian.PutUint32(buf[20:24], uint32(len(nameBytes)))
	copy(buf[24:], nameBytes)

	return buf
}

// MarshalFramebufferUpdateHeader builds the FramebufferUpdate header + rect
// header bytes for testing, without pixel data.
func MarshalFramebufferUpdateHeader(x, y, width, height int) []byte {
	var buf [16]byte
	// FramebufferUpdate: type(0) + pad(0) + nrects(1)
	buf[0] = 0
	buf[1] = 0
	binary.BigEndian.PutUint16(buf[2:4], 1)
	// Rect: x, y, w, h, encoding=0
	binary.BigEndian.PutUint16(buf[4:6], uint16(x))
	binary.BigEndian.PutUint16(buf[6:8], uint16(y))
	binary.BigEndian.PutUint16(buf[8:10], uint16(width))
	binary.BigEndian.PutUint16(buf[10:12], uint16(height))
	binary.BigEndian.PutUint32(buf[12:16], 0)
	return buf[:]
}

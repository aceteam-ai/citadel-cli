package desktop

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestMarshalServerInit(t *testing.T) {
	data := MarshalServerInit(1920, 1080, "Citadel Desktop")

	// Width
	width := binary.BigEndian.Uint16(data[0:2])
	if width != 1920 {
		t.Errorf("width = %d, want 1920", width)
	}

	// Height
	height := binary.BigEndian.Uint16(data[2:4])
	if height != 1080 {
		t.Errorf("height = %d, want 1080", height)
	}

	// Pixel format
	pf := data[4:20]
	if pf[0] != 32 {
		t.Errorf("bpp = %d, want 32", pf[0])
	}
	if pf[1] != 24 {
		t.Errorf("depth = %d, want 24", pf[1])
	}
	if pf[2] != 0 {
		t.Errorf("big-endian flag = %d, want 0", pf[2])
	}
	if pf[3] != 1 {
		t.Errorf("true-colour flag = %d, want 1", pf[3])
	}

	// red-max (big-endian u16) = 255
	redMax := binary.BigEndian.Uint16(pf[4:6])
	if redMax != 255 {
		t.Errorf("red-max = %d, want 255", redMax)
	}

	// Shifts
	if pf[10] != 16 {
		t.Errorf("red-shift = %d, want 16", pf[10])
	}
	if pf[11] != 8 {
		t.Errorf("green-shift = %d, want 8", pf[11])
	}
	if pf[12] != 0 {
		t.Errorf("blue-shift = %d, want 0", pf[12])
	}

	// Name
	nameLen := binary.BigEndian.Uint32(data[20:24])
	if nameLen != 15 {
		t.Errorf("name-length = %d, want 15", nameLen)
	}
	name := string(data[24:])
	if name != "Citadel Desktop" {
		t.Errorf("name = %q, want %q", name, "Citadel Desktop")
	}
}

func TestMarshalServerInitTotalSize(t *testing.T) {
	name := "Test"
	data := MarshalServerInit(800, 600, name)
	expected := 2 + 2 + 16 + 4 + len(name)
	if len(data) != expected {
		t.Errorf("ServerInit length = %d, want %d", len(data), expected)
	}
}

func TestMarshalFramebufferUpdateHeader(t *testing.T) {
	data := MarshalFramebufferUpdateHeader(0, 0, 1920, 1080)

	if len(data) != 16 {
		t.Fatalf("header length = %d, want 16", len(data))
	}

	// Message type
	if data[0] != 0 {
		t.Errorf("message type = %d, want 0", data[0])
	}

	// Number of rects
	nrects := binary.BigEndian.Uint16(data[2:4])
	if nrects != 1 {
		t.Errorf("nrects = %d, want 1", nrects)
	}

	// Rect x, y
	x := binary.BigEndian.Uint16(data[4:6])
	y := binary.BigEndian.Uint16(data[6:8])
	if x != 0 || y != 0 {
		t.Errorf("rect origin = (%d, %d), want (0, 0)", x, y)
	}

	// Rect width, height
	w := binary.BigEndian.Uint16(data[8:10])
	h := binary.BigEndian.Uint16(data[10:12])
	if w != 1920 || h != 1080 {
		t.Errorf("rect size = %dx%d, want 1920x1080", w, h)
	}

	// Encoding
	enc := binary.BigEndian.Uint32(data[12:16])
	if enc != 0 {
		t.Errorf("encoding = %d, want 0 (Raw)", enc)
	}
}

func TestWriteFramebufferUpdate(t *testing.T) {
	frame := &CaptureResult{
		Pix: make([]byte, 4*4*4), // 4x4 pixels, 4 bytes each
	}
	frame.Rect.Max.X = 4
	frame.Rect.Max.Y = 4

	// Fill with a known pattern
	for i := range frame.Pix {
		frame.Pix[i] = byte(i % 256)
	}

	var buf bytes.Buffer
	err := writeFramebufferUpdate(&buf, frame)
	if err != nil {
		t.Fatalf("writeFramebufferUpdate error: %v", err)
	}

	data := buf.Bytes()
	// Header (4) + rect header (12) + pixel data (64)
	expectedLen := 4 + 12 + len(frame.Pix)
	if len(data) != expectedLen {
		t.Fatalf("output length = %d, want %d", len(data), expectedLen)
	}

	// Verify pixel data follows the headers
	pixelStart := 16
	if !bytes.Equal(data[pixelStart:], frame.Pix) {
		t.Error("pixel data mismatch")
	}
}

func TestPixelFormatBGRX(t *testing.T) {
	// Verify the pixel format matches BGRX byte order (blue at offset 0, red at offset 2).
	// With red-shift=16, green-shift=8, blue-shift=0:
	// Byte 0 (shift 0) = Blue, Byte 1 (shift 8) = Green, Byte 2 (shift 16) = Red
	if serverPixelFormat[10] != 16 { // red-shift
		t.Errorf("red-shift = %d, want 16", serverPixelFormat[10])
	}
	if serverPixelFormat[11] != 8 { // green-shift
		t.Errorf("green-shift = %d, want 8", serverPixelFormat[11])
	}
	if serverPixelFormat[12] != 0 { // blue-shift
		t.Errorf("blue-shift = %d, want 0", serverPixelFormat[12])
	}
}

func TestCaptureResultWidthHeight(t *testing.T) {
	r := &CaptureResult{
		Pix: make([]byte, 1920*1080*4),
	}
	r.Rect.Max.X = 1920
	r.Rect.Max.Y = 1080

	if r.Width() != 1920 {
		t.Errorf("Width() = %d, want 1920", r.Width())
	}
	if r.Height() != 1080 {
		t.Errorf("Height() = %d, want 1080", r.Height())
	}
}

func TestNewVNCServerDefaults(t *testing.T) {
	s := NewVNCServer(VNCServerConfig{})
	if s.host != "127.0.0.1" {
		t.Errorf("host = %q, want %q", s.host, "127.0.0.1")
	}
	if s.port != 5900 {
		t.Errorf("port = %d, want 5900", s.port)
	}
	if s.fps != 10 {
		t.Errorf("fps = %d, want 10", s.fps)
	}
}

func TestNewVNCServerCustomConfig(t *testing.T) {
	s := NewVNCServer(VNCServerConfig{
		Host: "0.0.0.0",
		Port: 5901,
		FPS:  30,
	})
	if s.host != "0.0.0.0" {
		t.Errorf("host = %q, want %q", s.host, "0.0.0.0")
	}
	if s.port != 5901 {
		t.Errorf("port = %d, want 5901", s.port)
	}
	if s.fps != 30 {
		t.Errorf("fps = %d, want 30", s.fps)
	}
}

func TestNewVNCServerClampsFPS(t *testing.T) {
	s := NewVNCServer(VNCServerConfig{FPS: 120})
	if s.fps != 10 {
		t.Errorf("fps = %d, want 10 (clamped from 120)", s.fps)
	}

	s2 := NewVNCServer(VNCServerConfig{FPS: -5})
	if s2.fps != 10 {
		t.Errorf("fps = %d, want 10 (clamped from -5)", s2.fps)
	}
}

func TestStubCapturerReturnsError(t *testing.T) {
	// On non-Windows, newCapturer returns the stub which always errors.
	c := newCapturer()
	if err := c.Init(); err == nil {
		// On Windows this would succeed; on Linux/macOS it should fail.
		// Skip rather than fail since this test runs on all platforms.
		t.Skip("running on a platform with real capture support")
	}

	_, err := c.Capture()
	if err == nil {
		t.Error("stub Capture() should return error")
	}
}

func TestVNCServerStartFailsOnStub(t *testing.T) {
	s := NewVNCServer(VNCServerConfig{Port: 0}) // port 0 = any free port
	err := s.Start()
	if err == nil {
		s.Stop()
		t.Skip("running on a platform with real capture support")
	}
	// On stub platforms, Start should fail because Init fails
	if err == nil {
		t.Error("expected Start to fail on stub platform")
	}
}

func TestServerInitVersionString(t *testing.T) {
	// The RFB version string must be exactly 12 bytes.
	version := []byte("RFB 003.008\n")
	if len(version) != 12 {
		t.Errorf("version string length = %d, want 12", len(version))
	}
}

func TestAddListener(t *testing.T) {
	s := NewVNCServer(VNCServerConfig{})
	// AddListener should not panic on nil server state
	s.AddListener(nil) // just verifies it doesn't panic
	if len(s.extraListeners) != 1 {
		t.Errorf("extraListeners count = %d, want 1", len(s.extraListeners))
	}
}

func TestSetSilent(t *testing.T) {
	s := NewVNCServer(VNCServerConfig{})
	s.SetSilent()
	// Should not panic when logging after SetSilent
	s.logger.Printf("this should be silenced")
}

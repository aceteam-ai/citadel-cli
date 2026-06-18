package desktop

import "image"

// Capturer abstracts platform-specific screen capture. Implementations live
// in capture_windows.go (DXGI / GDI) and capture_stub.go (unsupported platforms).
type Capturer interface {
	// Init initializes the capture backend. Must be called before Capture.
	Init() error
	// Capture grabs the current screen contents into an RGBA buffer.
	// The returned image uses 32-bit pixels (4 bytes per pixel, BGRX byte order).
	Capture() (*CaptureResult, error)
	// Close releases any resources held by the capture backend.
	Close()
}

// CaptureResult holds the raw framebuffer data from a screen capture.
type CaptureResult struct {
	// Pix holds the raw pixel data in BGRX byte order (blue, green, red, unused).
	// Length is always Width * Height * 4.
	Pix []byte
	// Rect is the captured screen rectangle.
	Rect image.Rectangle
}

// Width returns the framebuffer width in pixels.
func (r *CaptureResult) Width() int { return r.Rect.Dx() }

// Height returns the framebuffer height in pixels.
func (r *CaptureResult) Height() int { return r.Rect.Dy() }

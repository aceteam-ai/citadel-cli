//go:build !windows

package desktop

import "fmt"

// stubCapturer is returned on non-Windows platforms where screen capture
// is not yet implemented.
type stubCapturer struct{}

func newCapturer() Capturer { return &stubCapturer{} }

func (s *stubCapturer) Init() error {
	return fmt.Errorf("screen capture not supported on this platform")
}

func (s *stubCapturer) Capture() (*CaptureResult, error) {
	return nil, fmt.Errorf("screen capture not supported on this platform")
}

func (s *stubCapturer) Close() {}

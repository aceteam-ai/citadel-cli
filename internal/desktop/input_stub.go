//go:build !windows

package desktop

// sendKeyEvent is a no-op on non-Windows platforms.
func sendKeyEvent(keysym uint32, down bool) {}

// sendPointerEvent is a no-op on non-Windows platforms.
func sendPointerEvent(x, y int, buttonMask byte) {}

// VNCInputAvailable reports whether this build supports VNC input injection.
func VNCInputAvailable() bool { return false }

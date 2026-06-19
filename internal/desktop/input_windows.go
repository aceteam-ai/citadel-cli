//go:build windows

package desktop

import "unsafe"

// Windows input injection via SendInput for VNC key/pointer events.

var (
	procSendInput      = user32.NewProc("SendInput")
	procSetCursorPos   = user32.NewProc("SetCursorPos")
	procMouseEvent     = user32.NewProc("mouse_event")
)

const (
	inputMouse    = 0
	inputKeyboard = 1

	keyEventFKeyUp   = 0x0002
	keyEventFUnicode = 0x0004

	mouseEventFAbsolute  = 0x8000
	mouseEventFMove      = 0x0001
	mouseEventFLeftDown  = 0x0002
	mouseEventFLeftUp    = 0x0004
	mouseEventFRightDown = 0x0008
	mouseEventFRightUp   = 0x0010
	mouseEventFMiddleDown = 0x0020
	mouseEventFMiddleUp   = 0x0040
	mouseEventFWheel     = 0x0800

	wheelDelta = 120
)

// mouseInput is the MOUSEINPUT structure (part of INPUT union).
// MOUSEINPUT is the largest union member so no trailing padding is needed.
// sizeof(INPUT) on amd64 = 4 (type) + 4 (pad) + 32 (MOUSEINPUT) = 40.
type mouseInput struct {
	Type uint32
	Mi   struct {
		Dx        int32
		Dy        int32
		MouseData uint32
		Flags     uint32
		Time      uint32
		ExtraInfo uintptr
	}
}

// keybdInput is the KEYBDINPUT structure (part of INPUT union).
type keybdInput struct {
	Type uint32
	Ki   struct {
		Vk        uint16
		Scan      uint16
		Flags     uint32
		Time      uint32
		ExtraInfo uintptr
	}
	_ [8]byte // padding to match INPUT union size
}

// sendKeyEvent sends a key press or release via SendInput.
// down=true for press, false for release. keysym is the X11 keysym from VNC.
func sendKeyEvent(keysym uint32, down bool) {
	vk, ok := keysymToVK(keysym)
	if !ok {
		return // unsupported key
	}

	var input keybdInput
	input.Type = inputKeyboard
	input.Ki.Vk = vk
	if !down {
		input.Ki.Flags = keyEventFKeyUp
	}

	procSendInput.Call(1, uintptr(unsafe.Pointer(&input)), unsafe.Sizeof(input))
}

var lastButtonMask byte

// sendPointerEvent sends a mouse move + optional button press via SendInput.
// buttonMask follows the RFB spec: bit0=left, bit1=middle, bit2=right,
// bit3=scroll-up, bit4=scroll-down.
func sendPointerEvent(x, y int, buttonMask byte) {
	procSetCursorPos.Call(uintptr(x), uintptr(y))

	// Only send button down/up on state transitions to avoid phantom clicks.
	changed := buttonMask ^ lastButtonMask
	lastButtonMask = buttonMask

	var flags uint32
	var mouseData uint32

	if changed&0x01 != 0 {
		if buttonMask&0x01 != 0 {
			flags |= mouseEventFLeftDown
		} else {
			flags |= mouseEventFLeftUp
		}
	}
	if changed&0x02 != 0 {
		if buttonMask&0x02 != 0 {
			flags |= mouseEventFMiddleDown
		} else {
			flags |= mouseEventFMiddleUp
		}
	}
	if changed&0x04 != 0 {
		if buttonMask&0x04 != 0 {
			flags |= mouseEventFRightDown
		} else {
			flags |= mouseEventFRightUp
		}
	}
	if buttonMask&0x08 != 0 {
		flags |= mouseEventFWheel
		mouseData = uint32(wheelDelta)
	}
	if buttonMask&0x10 != 0 {
		flags |= mouseEventFWheel
		mouseData = ^uint32(wheelDelta) + 1
	}

	if flags != 0 {
		var input mouseInput
		input.Type = inputMouse
		input.Mi.Flags = flags
		input.Mi.MouseData = mouseData
		procSendInput.Call(1, uintptr(unsafe.Pointer(&input)), unsafe.Sizeof(input))
	}
}

// keysymToVK maps common X11 keysyms to Windows virtual key codes.
// Covers printable ASCII and common control/modifier keys.
func keysymToVK(keysym uint32) (uint16, bool) {
	// Printable ASCII: keysym == Unicode codepoint for 0x20..0x7E
	if keysym >= 0x20 && keysym <= 0x7E {
		return asciiToVK(byte(keysym)), true
	}

	switch keysym {
	// Function keys
	case 0xff08: return 0x08, true  // BackSpace -> VK_BACK
	case 0xff09: return 0x09, true  // Tab -> VK_TAB
	case 0xff0d: return 0x0D, true  // Return -> VK_RETURN
	case 0xff1b: return 0x1B, true  // Escape -> VK_ESCAPE
	case 0xff63: return 0x2D, true  // Insert -> VK_INSERT
	case 0xffff: return 0x2E, true  // Delete -> VK_DELETE
	case 0xff50: return 0x24, true  // Home -> VK_HOME
	case 0xff57: return 0x23, true  // End -> VK_END
	case 0xff55: return 0x21, true  // Page_Up -> VK_PRIOR
	case 0xff56: return 0x22, true  // Page_Down -> VK_NEXT
	case 0xff51: return 0x25, true  // Left -> VK_LEFT
	case 0xff52: return 0x26, true  // Up -> VK_UP
	case 0xff53: return 0x27, true  // Right -> VK_RIGHT
	case 0xff54: return 0x28, true  // Down -> VK_DOWN

	// Modifiers
	case 0xffe1: return 0xA0, true  // Shift_L -> VK_LSHIFT
	case 0xffe2: return 0xA1, true  // Shift_R -> VK_RSHIFT
	case 0xffe3: return 0xA2, true  // Control_L -> VK_LCONTROL
	case 0xffe4: return 0xA3, true  // Control_R -> VK_RCONTROL
	case 0xffe9: return 0xA4, true  // Alt_L -> VK_LMENU
	case 0xffea: return 0xA5, true  // Alt_R -> VK_RMENU
	case 0xffeb: return 0x5B, true  // Super_L -> VK_LWIN
	case 0xffec: return 0x5C, true  // Super_R -> VK_RWIN
	case 0xffe5: return 0x14, true  // Caps_Lock -> VK_CAPITAL

	// F-keys
	case 0xffbe: return 0x70, true  // F1 -> VK_F1
	case 0xffbf: return 0x71, true  // F2
	case 0xffc0: return 0x72, true  // F3
	case 0xffc1: return 0x73, true  // F4
	case 0xffc2: return 0x74, true  // F5
	case 0xffc3: return 0x75, true  // F6
	case 0xffc4: return 0x76, true  // F7
	case 0xffc5: return 0x77, true  // F8
	case 0xffc6: return 0x78, true  // F9
	case 0xffc7: return 0x79, true  // F10
	case 0xffc8: return 0x7A, true  // F11
	case 0xffc9: return 0x7B, true  // F12
	}

	return 0, false
}

// asciiToVK converts a printable ASCII byte to a Windows VK code.
// Letters are uppercased (VK codes are uppercase A-Z = 0x41-0x5A).
// Digits are 0x30-0x39. Other punctuation maps to their VK equivalents.
func asciiToVK(c byte) uint16 {
	if c >= 'a' && c <= 'z' {
		return uint16(c - 32) // lowercase -> uppercase
	}
	// For digits, uppercase letters, space, and most punctuation,
	// the VK code equals the ASCII code.
	return uint16(c)
}

// VNCInputAvailable reports whether this build supports VNC input injection.
func VNCInputAvailable() bool { return true }

// Compile-time size assertions: sizeof(INPUT) on amd64 must be 40.
var _ [40]byte = [unsafe.Sizeof(mouseInput{})]byte{}
var _ [40]byte = [unsafe.Sizeof(keybdInput{})]byte{}

//go:build windows

package desktop

import (
	"fmt"
	"image"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	shcore   = windows.NewLazySystemDLL("shcore.dll")

	procGetDC             = user32.NewProc("GetDC")
	procReleaseDC         = user32.NewProc("ReleaseDC")
	procGetSystemMetrics  = user32.NewProc("GetSystemMetrics")
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")

	procCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject           = gdi32.NewProc("SelectObject")
	procBitBlt                 = gdi32.NewProc("BitBlt")
	procDeleteObject           = gdi32.NewProc("DeleteObject")
	procDeleteDC               = gdi32.NewProc("DeleteDC")
	procGetDIBits              = gdi32.NewProc("GetDIBits")

	procSetProcessDpiAwareness = shcore.NewProc("SetProcessDpiAwareness")
)

const (
	smCxScreen = 0  // SM_CXSCREEN
	smCyScreen = 1  // SM_CYSCREEN
	srccopy    = 0x00CC0020
	biRGB      = 0
	dibRGBColors = 0
)

// bitmapInfoHeader is the BITMAPINFOHEADER structure.
type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

// gdiCapturer captures the screen using GDI BitBlt.
type gdiCapturer struct {
	width  int
	height int
}

func newCapturer() Capturer {
	return &gdiCapturer{}
}

func (g *gdiCapturer) Init() error {
	// Set DPI awareness so GetSystemMetrics returns physical pixels.
	// Try per-monitor V2 first (Win10 1703+), fall back to per-monitor V1.
	// -4 = DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2
	if procSetProcessDpiAwarenessContext.Find() == nil {
		procSetProcessDpiAwarenessContext.Call(uintptr(^uintptr(3))) // -4 as uintptr
	} else if procSetProcessDpiAwareness.Find() == nil {
		// PROCESS_PER_MONITOR_DPI_AWARE = 2
		procSetProcessDpiAwareness.Call(2)
	}

	w, _, _ := procGetSystemMetrics.Call(smCxScreen)
	h, _, _ := procGetSystemMetrics.Call(smCyScreen)
	if w == 0 || h == 0 {
		return fmt.Errorf("GetSystemMetrics returned 0x0 — no display attached")
	}
	g.width = int(w)
	g.height = int(h)
	return nil
}

func (g *gdiCapturer) Capture() (*CaptureResult, error) {
	if g.width == 0 || g.height == 0 {
		if err := g.Init(); err != nil {
			return nil, err
		}
	}

	// Re-read screen size each frame in case resolution changed.
	w, _, _ := procGetSystemMetrics.Call(smCxScreen)
	h, _, _ := procGetSystemMetrics.Call(smCyScreen)
	if w == 0 || h == 0 {
		return nil, fmt.Errorf("GetSystemMetrics returned 0x0")
	}
	g.width = int(w)
	g.height = int(h)

	// Get the screen DC
	hdcScreen, _, _ := procGetDC.Call(0) // NULL = entire screen
	if hdcScreen == 0 {
		return nil, fmt.Errorf("GetDC(NULL) failed")
	}
	defer procReleaseDC.Call(0, hdcScreen)

	// Create a compatible DC and bitmap
	hdcMem, _, _ := procCreateCompatibleDC.Call(hdcScreen)
	if hdcMem == 0 {
		return nil, fmt.Errorf("CreateCompatibleDC failed")
	}
	defer procDeleteDC.Call(hdcMem)

	hBitmap, _, _ := procCreateCompatibleBitmap.Call(hdcScreen, uintptr(g.width), uintptr(g.height))
	if hBitmap == 0 {
		return nil, fmt.Errorf("CreateCompatibleBitmap failed")
	}
	defer procDeleteObject.Call(hBitmap)

	// Select the bitmap into the memory DC
	procSelectObject.Call(hdcMem, hBitmap)

	// BitBlt: copy from screen DC to memory DC
	ret, _, _ := procBitBlt.Call(
		hdcMem, 0, 0, uintptr(g.width), uintptr(g.height),
		hdcScreen, 0, 0, srccopy,
	)
	if ret == 0 {
		return nil, fmt.Errorf("BitBlt failed")
	}

	// Prepare BITMAPINFO for GetDIBits
	bmi := bitmapInfoHeader{
		Size:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		Width:       int32(g.width),
		Height:      -int32(g.height), // negative = top-down DIB
		Planes:      1,
		BitCount:    32,
		Compression: biRGB,
	}

	// Allocate pixel buffer
	pixels := make([]byte, g.width*g.height*4)

	ret, _, _ = procGetDIBits.Call(
		hdcMem,
		hBitmap,
		0,
		uintptr(g.height),
		uintptr(unsafe.Pointer(&pixels[0])),
		uintptr(unsafe.Pointer(&bmi)),
		dibRGBColors,
	)
	if ret == 0 {
		return nil, fmt.Errorf("GetDIBits failed")
	}

	return &CaptureResult{
		Pix:  pixels,
		Rect: image.Rect(0, 0, g.width, g.height),
	}, nil
}

func (g *gdiCapturer) Close() {}

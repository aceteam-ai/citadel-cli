//go:build linux

package pairingdisplay

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Mechanism (design doc §9.1): resolve the active VT via the kernel-owned,
// world-readable /sys/class/tty/tty0/active indirection, confirm it is in
// KD_TEXT mode (not owned by a graphical/X/Wayland session) via KDGETMODE,
// then write() an ANSI frame directly to the character device. This
// deliberately bypasses os.Stdout/journald entirely (internal/worker/
// pairing_display.go's package doc has the full security invariant).
//
// KDGETMODE is the load-bearing check, NOT internal/session.DetectDesktop():
// that reads DISPLAY/WAYLAND_DISPLAY from the WORKER'S OWN environment, and
// the fleet worker is a root systemd unit with neither var set even when a
// desktop session owns the seat for some other user — from that process,
// DetectDesktop reports "headless" unconditionally, while KDGETMODE asks the
// kernel what the seat is actually doing. Writing text to a KD_GRAPHICS VT
// succeeds and is invisible: the exact false-`delivered:true` this package
// must never produce.
const (
	ttyActivePath   = "/sys/class/tty/tty0/active"
	ttyDevicePrefix = "/dev/"

	// KDGETMODE / KD_TEXT are Linux console ioctl constants
	// (linux/kd.h). Not present in golang.org/x/sys/unix as named
	// constants, so they are pinned here.
	kdGetMode = 0x4B3B
	kdText    = 0x00
)

func newPlatformRenderer() Renderer {
	return consoleRenderer{}
}

// consoleRenderer implements Renderer by writing directly to the active
// Linux virtual-terminal character device.
type consoleRenderer struct{}

func (consoleRenderer) ResolveTarget() (target string, reason string, ok bool) {
	device, resolved := resolveActiveVTDevice()
	if !resolved {
		return "", "no_console", false
	}
	return device, "", true
}

func (consoleRenderer) Show(target string, req ShowRequest) RenderResult {
	f, reason := openTextConsole(target)
	if reason != "" {
		return RenderResult{Reason: reason}
	}
	defer f.Close()

	if _, err := f.WriteString(renderShowFrame(req)); err != nil {
		return RenderResult{Reason: "render_error"}
	}
	return RenderResult{Delivered: true, Surface: "console"}
}

func (consoleRenderer) Clear(target string, note string) error {
	f, reason := openTextConsole(target)
	if reason != "" {
		return fmt.Errorf("clear %s: %s", target, reason)
	}
	defer f.Close()
	if _, err := f.WriteString(renderClearFrame(note)); err != nil {
		return fmt.Errorf("clear %s: %w", target, err)
	}
	return nil
}

func (consoleRenderer) DetectSurfaces() []string {
	device, ok := resolveActiveVTDevice()
	if !ok {
		return nil
	}
	f, reason := openTextConsole(device)
	if reason != "" {
		return nil
	}
	_ = f.Close()
	return []string{"console"}
}

// resolveActiveVTDevice reads the kernel-owned active-VT indirection and
// maps it to a device path. It does not open the device or attempt an
// ioctl — a VT can flip between resolve and use, so that check happens at
// use time (openTextConsole).
func resolveActiveVTDevice() (string, bool) {
	raw, err := os.ReadFile(ttyActivePath)
	if err != nil {
		return "", false
	}
	name := strings.TrimSpace(string(raw))
	if name == "" {
		return "", false
	}
	return ttyDevicePrefix + name, true
}

// openTextConsole opens device for read-write (matching getty's own open
// mode — KDGETMODE needs a valid fd on the console) and confirms it is in
// KD_TEXT mode. On any failure it returns a nil file and a §8.3 reason
// string; the caller must fail closed (delivered:false) on every one of
// them.
func openTextConsole(device string) (*os.File, string) {
	f, err := os.OpenFile(device, os.O_RDWR, 0)
	if err != nil {
		if os.IsPermission(err) {
			return nil, "permission_denied"
		}
		return nil, "no_console"
	}
	mode, err := unix.IoctlGetInt(int(f.Fd()), kdGetMode)
	if err != nil || mode != kdText {
		_ = f.Close()
		return nil, "graphical_session"
	}
	return f, ""
}

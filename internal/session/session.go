// Package session detects whether the current process has access to an
// interactive desktop session, and which desktop-dependent sub-capabilities
// (VNC, screenshot, input injection, desktop-attached terminal) are usable.
//
// The motivation: Citadel advertises desktop features unconditionally, but
// whether they actually work depends on how the process was launched. A process
// started over SSH, from a TTY with no DISPLAY, or as a Windows service in
// Session 0 has no interactive desktop and will silently fail VNC/screenshot/
// input actions. Detecting this up front lets the TUI surface the limitation
// and lets the node advertise an honest capability map in its handshake.
//
// Detection is per-OS and CGO-free (env checks + golang.org/x/sys syscalls +
// small subprocess probes), implemented in build-tagged files.
package session

// DesktopInfo is the structured result of a desktop-session probe. It answers
// "do I have an interactive desktop session?" plus which sub-capabilities are
// available, and a human-readable reason explaining the verdict.
type DesktopInfo struct {
	// HasDesktop is true when the process can drive an interactive desktop
	// (window server / display server reachable).
	HasDesktop bool `json:"has_desktop" yaml:"has_desktop"`

	// Reason is a short human-readable explanation of the verdict, suitable for
	// display in the TUI (e.g. "DISPLAY=:0", "headless session (no DISPLAY/WAYLAND_DISPLAY)").
	Reason string `json:"reason" yaml:"reason"`

	// SessionType describes how the process is attached to a session
	// (e.g. "x11", "wayland", "aqua", "ssh", "console", "session0", "headless").
	SessionType string `json:"session_type,omitempty" yaml:"session_type,omitempty"`

	// Sub-capabilities derived from HasDesktop and platform support. These mirror
	// the desktop affordances the server/frontend gate on.
	VNC        bool `json:"vnc" yaml:"vnc"`                         // remote desktop streaming
	Screenshot bool `json:"screenshot" yaml:"screenshot"`           // screen capture
	Input      bool `json:"input_injection" yaml:"input_injection"` // synthetic key/pointer events
	Terminal   bool `json:"terminal" yaml:"terminal"`               // desktop-attached terminal (always available)
}

// DetectDesktop probes the current process for interactive desktop access. It
// never returns nil and never errors: an undetectable or headless environment
// yields HasDesktop=false with an explanatory Reason. The per-OS implementation
// lives in the build-tagged session_<os>.go files.
func DetectDesktop() *DesktopInfo {
	info := detectDesktop()
	info.applyDerived()
	return info
}

// applyDerived fills the sub-capability booleans from HasDesktop. A
// desktop-attached terminal is treated as always available (the node can run a
// PTY regardless of a window server), while VNC/screenshot/input require an
// interactive desktop. This keeps the derivation in one place so the per-OS
// probes only need to determine HasDesktop, SessionType, and Reason.
func (d *DesktopInfo) applyDerived() {
	d.Terminal = true
	d.VNC = d.HasDesktop
	d.Screenshot = d.HasDesktop
	d.Input = d.HasDesktop
}

// CapabilityMap returns the desktop capabilities as a string->bool map suitable
// for embedding in the node registration/heartbeat handshake. Keys are stable
// wire identifiers. The map is additive: consumers that don't recognise a key
// ignore it, and a node that never reports the map is treated as "unknown"
// (legacy behaviour) by the server.
func (d *DesktopInfo) CapabilityMap() map[string]bool {
	return map[string]bool{
		"desktop":         d.HasDesktop,
		"vnc":             d.VNC,
		"screenshot":      d.Screenshot,
		"input_injection": d.Input,
		"terminal":        d.Terminal,
	}
}

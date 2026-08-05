//go:build windows

// internal/network/tun_windows.go
// Windows-specific TUN identity (issue #643).
package network

import (
	"github.com/dblohm7/wingoes/com"
	"github.com/tailscale/wireguard-go/tun"
	"golang.org/x/sys/windows"
)

// Wintun identifies an adapter by GUID, and tailscale.com/net/tstun's init
// pins BOTH the tunnel type and the adapter GUID to Tailscale's own values:
//
//	tun.WintunTunnelType = "Tailscale"
//	tun.WintunStaticRequestedGUID = {37217669-42da-4657-a55b-0d995d328250}
//
// Any process using tstun therefore asks for the SAME adapter. Verified on a
// Windows 11 box with Tailscale installed: `citadel up --check` failed with
// "Cannot create a file when that file already exists" (0x800700B7) because
// Tailscale already owned that GUID. The interface name we pass is irrelevant
// — the collision is on the GUID.
//
// Citadel claims its own adapter identity so machine-wide mode can coexist
// with an installed Tailscale. The GUID is fixed rather than random so a
// restart re-attaches to citadel's existing adapter instead of leaking a new
// one on every run.
//
// This init runs after tstun's (Go initializes dependencies first), so it
// overrides rather than races.
func init() {
	tun.WintunTunnelType = "Citadel"
	guid, err := windows.GUIDFromString("{c17ade10-9c5b-4b8e-9f0d-7c3a1e5d6b21}")
	if err != nil {
		// Unreachable with a literal GUID; leaving tstun's default would
		// silently collide with Tailscale, so fail loudly at startup.
		panic("citadel: invalid Wintun GUID: " + err.Error())
	}
	tun.WintunStaticRequestedGUID = &guid
}

// COM must be initialized process-wide before the Windows router runs.
//
// osrouter's setPrivateNetwork (which marks citadel's adapter as a Private
// network, so Windows Firewall does not treat mesh peers as untrusted) builds
// an `ole.Connection` with the comment "DO NOT call Initialize() ... We've
// already handled that process-wide". tailscaled does that in its own init
// (cmd/tailscaled/tailscaled_windows.go); nothing in the library does it for
// you.
//
// Without this, every attempt fails with "CoInitialize has not been called"
// and after 20 tries the router gives up:
//
//	setPrivateNetwork: adapter LUID ... not found after 20 tries, giving up
//
// Verified on the Windows 11 test VM. The bring-up survives it — the adapter
// exists and the engine starts — so this is a silent misconfiguration rather
// than a crash: the interface would be left in the Public firewall profile.
//
// ConsoleApp is the right process type: `citadel up` runs in the foreground.
// A future Windows service wrapper should pass com.Service instead.
func init() {
	if err := com.StartRuntime(com.ConsoleApp); err != nil {
		logf("windows: COM runtime init failed (adapter may stay in the Public firewall profile): %v", err)
	}
}

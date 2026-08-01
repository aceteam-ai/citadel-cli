//go:build windows

// internal/network/tun_windows.go
// Windows-specific TUN identity (issue #643).
package network

import (
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

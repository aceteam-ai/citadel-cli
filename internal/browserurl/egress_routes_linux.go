//go:build linux

package browserurl

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"os"
)

// osOnLinkRoutes reads the main-table routes from procfs. Either family may
// be missing (IPv6 disabled); only both failing is an error.
func osOnLinkRoutes() ([]netip.Prefix, error) {
	v4, err4 := os.ReadFile("/proc/net/route")
	v6, err6 := os.ReadFile("/proc/net/ipv6_route")
	if err4 != nil && err6 != nil {
		return nil, errors.Join(err4, err6)
	}
	var out []netip.Prefix
	if err4 == nil {
		out = append(out, parseLinuxIPv4Routes(string(v4), binary.NativeEndian)...)
	}
	if err6 == nil {
		out = append(out, parseLinuxIPv6Routes(string(v6))...)
	}
	return out, nil
}

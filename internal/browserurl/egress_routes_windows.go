//go:build windows

package browserurl

import (
	"net/netip"
	"unsafe"

	"golang.org/x/sys/windows"
)

// osOnLinkRoutes reads the IPv4 and IPv6 route tables (GetIpForwardTable2)
// and keeps the routes whose next hop is unspecified (directly connected).
func osOnLinkRoutes() ([]netip.Prefix, error) {
	var table *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(windows.AF_UNSPEC, &table); err != nil {
		return nil, err
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))
	var out []netip.Prefix
	for _, row := range table.Rows() {
		dst := *(*[28]byte)(unsafe.Pointer(&row.DestinationPrefix.Prefix))
		nh := *(*[28]byte)(unsafe.Pointer(&row.NextHop))
		if p, ok := windowsOnLinkRoute(dst, row.DestinationPrefix.PrefixLength, nh); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

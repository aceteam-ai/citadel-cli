//go:build !linux && !windows && !darwin

package browserurl

import (
	"errors"
	"net/netip"
)

// osOnLinkRoutes has no reader on this OS; the guard falls back to the
// interface masks.
func osOnLinkRoutes() ([]netip.Prefix, error) {
	return nil, errors.New("route table not read on this OS")
}

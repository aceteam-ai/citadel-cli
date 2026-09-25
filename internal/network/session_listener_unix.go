//go:build !windows

package network

import (
	"context"
	"net"
)

func dialLocalControlRaw(ctx context.Context, path string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}

// Do not use safesocket.Listen here: its Unix implementation unlinks the
// socket after any failed dial, including a transiently busy live listener.
// A direct bind preserves the exact shared socket path and fails closed when
// another owner appears between our stale check and Listen.
func listenSessionControlEndpoint(path string) (net.Listener, error) {
	return net.Listen("unix", path)
}

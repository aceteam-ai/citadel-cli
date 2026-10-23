//go:build !linux

package declaredsession

import (
	"context"
	"net"
)

type holderListener struct{}

func listenHolder(*SessionStore) (*holderListener, error) {
	return nil, ErrUnsupported
}
func (*holderListener) Accept() (net.Conn, error) {
	return nil, ErrUnsupported
}
func (*holderListener) Close() error {
	return ErrUnsupported
}
func (*holderListener) stopAccept() {}
func sameUIDPeer(net.Conn) (int, error) {
	return 0, ErrUnsupported
}
func dialHolderSocket(context.Context, *SessionStore) (net.Conn, int, error) {
	return nil, 0, ErrUnsupported
}

func observeHolderPeer(int) (ProcessIdentity, error) { return ProcessIdentity{}, ErrUnsupported }

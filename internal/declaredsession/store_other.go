//go:build !linux

package declaredsession

import (
	"context"
	"os"
)

func OpenStore(string) (*Store, error) {
	return nil, ErrUnsupported
}

func (*Store) OpenSession(string, bool) (*SessionStore, error) {
	return nil, ErrUnsupported
}

func (*SessionStore) SocketAnchor() (*os.File, error) {
	return nil, ErrUnsupported
}

func readPrivateAt(*os.File, string, int, uint32) ([]byte, bool, error) {
	return nil, false, ErrUnsupported
}

func readPrivatePath(string, int, uint32) ([]byte, error) {
	return nil, ErrUnsupported
}

func writePrivateAt(*os.File, string, []byte, int, uint32) error {
	return ErrUnsupported
}

func removePrivateAt(*os.File, string, uint32) error {
	return ErrUnsupported
}

func (*SessionStore) acquire(context.Context, string) (*fileGuard, error) {
	return nil, ErrUnsupported
}

func (*fileGuard) close() error {
	return ErrUnsupported
}

func closePrivate(*os.File) error {
	return ErrUnsupported
}

func appendLogAt(*os.File, string, []byte, LogPolicy, uint32) error {
	return ErrUnsupported
}

func readLogTailAt(*os.File, string, int, uint32) ([]byte, error) {
	return nil, ErrUnsupported
}

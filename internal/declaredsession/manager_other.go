//go:build !linux

package declaredsession

import (
	"context"

	"github.com/aceteam-ai/citadel-cli/internal/procscope"
)

func nativeManagerOps() managerOps {
	return managerOps{
		inspect: func(context.Context, string) (procscope.Observation, error) {
			return procscope.Observation{}, ErrUnsupported
		},
		process: procscope.ObserveProcess,
		stop:    procscope.StopExact,
		dial: func(context.Context, *SessionStore) (managerPeer, error) {
			return nil, ErrUnsupported
		},
		attachDial: func(context.Context, *SessionStore) (*Client, error) {
			return nil, ErrUnsupported
		},
		socket: func(*SessionStore) (managerSocket, error) {
			return managerSocket{}, ErrUnsupported
		},
		removeSocket: func(*SessionStore, managerSocket) error {
			return ErrUnsupported
		},
		launch: func(context.Context, string, string, string, string, string, bool) (int, error) {
			return 0, ErrUnsupported
		},
	}
}

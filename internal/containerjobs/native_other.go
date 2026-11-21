//go:build !linux

package containerjobs

import (
	"context"
	"os"
)

type Native struct{}

func NewNative(string, func() string) *Native                        { return &Native{} }
func NewNativeWithPolicy(string, func() (HostPolicy, error)) *Native { return &Native{} }
func NativeSupported() bool                                          { return false }
func (*Native) Lock(context.Context) (func(), error)                 { return nil, ErrUnavailable }
func (*Native) Container(context.Context, string) (ContainerState, error) {
	return ContainerState{}, ErrUnavailable
}
func (*Native) Volume(context.Context, string) (VolumeState, error) {
	return VolumeState{}, ErrUnavailable
}
func (*Native) CreateVolume(context.Context, Request) error    { return ErrUnavailable }
func (*Native) CreateContainer(context.Context, Request) error { return ErrUnavailable }
func (*Native) Start(context.Context, string) error            { return ErrUnavailable }
func (*Native) Stop(context.Context, string) error             { return ErrUnavailable }
func (*Native) Exec(context.Context, string, []string) (ExecResult, error) {
	return ExecResult{}, ErrUnavailable
}
func (*Native) ClaimExecution(context.Context, string, string) error { return ErrUnavailable }

func (*Native) OpenVolume(context.Context, string) (*os.Root, error) { return nil, ErrUnavailable }
func (*Native) Usage(context.Context, string) (int64, int64, error)  { return 0, 0, ErrUnavailable }
func (*Native) DestroyContainer(context.Context, string) error       { return ErrUnavailable }
func (*Native) DestroyVolume(context.Context, string) error          { return ErrUnavailable }

//go:build !linux

package reconstruct

import (
	"context"
	"fmt"
)

type osExecutor struct{}

// NewOSExecutor compiles on unsupported platforms but fails closed: the
// pinned Vulkan image and its process-group cancellation contract are Linux-only.
func NewOSExecutor() Executor { return osExecutor{} }

func (osExecutor) Run(context.Context, Invocation) error {
	return fmt.Errorf("reconstruct: Spirula executor is supported only in the Linux GPU container")
}

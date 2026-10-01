//go:build !windows

package reconstruct

import (
	"context"
	"testing"
)

func TestOSExecutorConfiguresDedicatedProcessGroupCancellation(t *testing.T) {
	t.Parallel()
	cmd := newOSCommand(context.Background(), Invocation{Name: "unused"})
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("child must launch in a dedicated process group")
	}
	if cmd.Cancel == nil {
		t.Fatal("context cancellation must kill the process group")
	}
	if cmd.WaitDelay == 0 {
		t.Fatal("executor must bound waits on broken child pipes")
	}
}

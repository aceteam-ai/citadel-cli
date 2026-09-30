//go:build windows

package cmd

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestConfigurePapercraftProcessTreeWindows(t *testing.T) {
	if _, err := newRenderLimitedCommand(context.Background(), nil, "unused.exe"); err == nil || !strings.Contains(err.Error(), "unsupported on windows") {
		t.Fatalf("non-Linux render command error = %v", err)
	}
	processTree := configurePapercraftProcessTree(&renderCommand{cmd: exec.Command("unused.exe")})
	if err := processTree.cleanupAfterWait(); err == nil || !strings.Contains(err.Error(), "unsupported on Windows") {
		t.Fatalf("dead Windows process stub error = %v", err)
	}
}

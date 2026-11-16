//go:build windows

package localgate

import (
	"os/exec"
	"strconv"
)

func setProcessGroup(*exec.Cmd) {}

// killTree ends citadel and every process it started, such as the browser.
func killTree(c *exec.Cmd) {
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(c.Process.Pid)).Run()
	_ = c.Process.Kill()
}

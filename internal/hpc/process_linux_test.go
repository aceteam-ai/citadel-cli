//go:build linux

package hpc

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestStopGroupReapsHelpers starts a shell that leaves a background helper
// (as a model server's worker processes would) and checks StopGroup ends both.
func TestStopGroupReapsHelpers(t *testing.T) {
	r, w := io.Pipe()
	p, err := StartProcess([]string{"/bin/sh", "-c", "trap '' TERM; sleep 60 & echo $!; wait"}, nil, w, true)
	if err != nil {
		t.Skipf("no /bin/sh: %v", err)
	}
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	helper, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, r) }()
	StopGroup(p, 200*time.Millisecond)
	w.Close()
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(helper, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("helper %d survived the group stop", helper)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if syscall.Kill(p.Pid(), 0) == nil {
		t.Fatalf("leader %d survived", p.Pid())
	}
}

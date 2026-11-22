package platform

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A fake test executable, never a node/daemon/lane process. Its only input is
// an owned temporary registry. The parent kills exactly this child and proves
// the OS lock is released without deleting/replacing its stable lock inode.
func TestSuspendStoreHelperProcess(t *testing.T) {
	if os.Getenv("CITADEL_SUSPEND_STORE_TEST_HELPER") != "1" {
		t.Skip("private child fixture only")
	}
	s, err := openSuspendManifestStore(os.Getenv("CITADEL_SUSPEND_STORE_TEST_REGISTRY"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	fmt.Print("ready")
	var command [1]byte
	_, _ = os.Stdin.Read(command[:])
}

func TestSuspendStoreCrossProcessLockAndCrashRelease(t *testing.T) {
	reg := filepath.Join(t.TempDir(), "registry")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestSuspendStoreHelperProcess$")
	cmd.Env = append(os.Environ(), "CITADEL_SUSPEND_STORE_TEST_HELPER=1", "CITADEL_SUSPEND_STORE_TEST_REGISTRY="+reg)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	ready := make(chan error, 1)
	go func() {
		var b [5]byte
		_, err := io.ReadFull(stdout, b[:])
		if err == nil && string(b[:]) != "ready" {
			err = fmt.Errorf("unexpected private child receipt")
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("private child lock probe timed out")
	}
	if other, err := openSuspendManifestStore(reg, false); err == nil {
		_ = other.close()
		t.Fatal("another process acquired the live store")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("private crash fixture unexpectedly exited cleanly")
	}
	other, err := openSuspendManifestStore(reg, false)
	if err != nil {
		t.Fatal("crashed process left the store locked")
	}
	defer other.close()
}

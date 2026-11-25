package compose

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyRemovalExcludesReplacementBackup(t *testing.T) {
	out := []byte("old-id citadel-tei\nbackup-id citadel-tei-citadel-prev\n")
	got := legacyRemovalIDs(out, BackupContainerName("citadel-tei"))
	if !reflect.DeepEqual(got, []string{"old-id"}) {
		t.Fatalf("legacy cleanup selected %v; backup must remain recoverable", got)
	}
}

// The cleanup and replacement processes must agree on a lock before cleanup
// can list an ID that replacement might rename into a recoverable backup.
func TestLegacyMutationLockCrossProcess(t *testing.T) {
	if os.Getenv("CITADEL_TEST_COMPOSE_LOCK_HELPER") == "1" {
		err := withLegacyMutationLockAt(os.Getenv("CITADEL_TEST_COMPOSE_LOCK_ROOT"), "docker", "tei", func() error {
			_, _ = io.WriteString(os.Stdout, "LOCKED\n")
			_, _ = io.Copy(io.Discard, os.Stdin)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("APPDATA", root)
	cmd := exec.Command(os.Args[0], "-test.run=^TestLegacyMutationLockCrossProcess$")
	cmd.Env = append(os.Environ(), "CITADEL_TEST_COMPOSE_LOCK_HELPER=1", "CITADEL_TEST_COMPOSE_LOCK_ROOT="+root)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "LOCKED" {
		t.Fatalf("helper did not acquire lock: %q, %v", line, err)
	}
	called := false
	err = withLegacyMutationLock("docker", "tei", func() error { called = true; return nil })
	if err == nil || called {
		t.Fatalf("second process entered the backup window: called=%v err=%v", called, err)
	}
	f := newReplaceFixture(t)
	f.addOld("citadel-tei", "running")
	f.refresh(t)
	if outcome, replaceErr := ReplaceRunningService(f.spec(nil)); replaceErr == nil || outcome != ReplaceUnchanged || f.restored != 1 || len(f.engine.Calls) != 0 {
		t.Fatalf("replacement touched a container while another process held the lock: outcome=%v err=%v restores=%d calls=%v", outcome, replaceErr, f.restored, f.engine.Calls)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	// A completed replacement no longer blocks the next cleanup.
	if err := withLegacyMutationLock("docker", "tei", func() error { called = true; return nil }); err != nil || !called {
		t.Fatalf("lock did not release: called=%v err=%v", called, err)
	}
}

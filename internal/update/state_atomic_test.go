package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStateReplacementPreservesOpenReaderAndRestrictiveMode(t *testing.T) {
	isolateState(t)
	if err := SaveState(&State{AutoUpdate: true, Channel: strings.Repeat("long-old-value", 256)}); err != nil {
		t.Fatal(err)
	}
	path := GetStateFilePath()
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := SaveState(&State{Channel: "short"}); err != nil {
		t.Fatal(err)
	}
	old, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(old, before) {
		t.Fatal("replacement changed a reader's already-open snapshot")
	}
	state, err := LoadState()
	if err != nil || state.Channel != "short" || state.AutoUpdate {
		t.Fatalf("new snapshot not published: state=%+v err=%v", state, err)
	}
	info, err := os.Stat(path)
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("restrictive state mode not preserved: %v", err)
	}
}

func TestStateWriteFailureBoundaries(t *testing.T) {
	for _, stage := range []string{"file sync", "rename", "directory sync"} {
		t.Run(stage, func(t *testing.T) {
			isolateState(t)
			if err := SaveState(&State{Channel: "old"}); err != nil {
				t.Fatal(err)
			}
			path := GetStateFilePath()
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			injected := errors.New("fixture write failure")
			ops := defaultStateFileOps()
			switch stage {
			case "file sync":
				ops.syncFile = func(*os.File) error { return injected }
			case "rename":
				ops.rename = func(string, string) error { return injected }
			case "directory sync":
				ops.syncDir = func(string) error { return injected }
			}
			err = withStateLock(path, func() error { return writeStateFile(path, &State{Channel: "new"}, ops) })
			if !errors.Is(err, injected) {
				t.Fatalf("failure not returned: %v", err)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if stage == "directory sync" {
				state, decodeErr := LoadState()
				if !strings.Contains(err.Error(), "durability unknown") || decodeErr != nil || state.Channel != "new" {
					t.Fatalf("published but unconfirmed state misreported: state=%+v err=%v", state, err)
				}
			} else if !bytes.Equal(before, after) {
				t.Fatal("pre-publication failure changed the previous state")
			}
			leftovers, globErr := filepath.Glob(filepath.Join(GetUpdateDir(), ".state.json.tmp-*"))
			if globErr != nil || len(leftovers) != 0 {
				t.Fatalf("failed save leaked temporary state: %v %v", leftovers, globErr)
			}
		})
	}
}

func TestCorruptStateNeedsExplicitPreferenceRepair(t *testing.T) {
	for _, broken := range []string{"{\"auto_update\":true}}", "null", ""} {
		t.Run(strconv.Itoa(len(broken)), func(t *testing.T) {
			isolateState(t)
			if err := EnsureUpdateDir(); err != nil {
				t.Fatal(err)
			}
			path := GetStateFilePath()
			if err := os.WriteFile(path, []byte(broken), 0600); err != nil {
				t.Fatal(err)
			}
			if state, err := LoadState(); state != nil || !errors.Is(err, ErrStateCorrupt) {
				t.Fatalf("corruption became a confident default: state=%+v err=%v", state, err)
			}
			if err := RecordCheckResult(time.Unix(1, 0), ResultCheckFailed, true, ""); !errors.Is(err, ErrStateCorrupt) {
				t.Fatalf("ordinary telemetry repaired corrupt state: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != broken {
				t.Fatal("telemetry overwrote corrupt-state evidence")
			}
			if err := SetAutoUpdate(false); err != nil {
				t.Fatal(err)
			}
			backups, err := filepath.Glob(path + ".corrupt-*")
			if err != nil || len(backups) != 1 {
				t.Fatalf("explicit repair lost evidence: backups=%v err=%v", backups, err)
			}
			if data, err := os.ReadFile(backups[0]); err != nil || string(data) != broken {
				t.Fatalf("backup did not preserve exact malformed bytes: %q err=%v", data, err)
			}
			if info, err := os.Stat(backups[0]); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
				t.Fatalf("backup is not private: %v", err)
			}
			state, err := LoadState()
			if err != nil || state.AutoUpdate || state.Channel != "stable" || state.LastResult != "" {
				t.Fatalf("explicit disable guessed previous fields: state=%+v err=%v", state, err)
			}
			if err := SetAutoUpdate(true); err != nil {
				t.Fatal(err)
			}
			state, err = LoadState()
			if err != nil || !state.AutoUpdate {
				t.Fatalf("explicit enable failed: state=%+v err=%v", state, err)
			}
			if after, err := filepath.Glob(path + ".corrupt-*"); err != nil || len(after) != 1 {
				t.Fatalf("valid state created another corrupt backup: %v %v", after, err)
			}
		})
	}
}

func TestPreferenceRepairDoesNotReplaceUnreadableState(t *testing.T) {
	isolateState(t)
	if err := os.MkdirAll(GetStateFilePath(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := SetAutoUpdate(true); err == nil {
		t.Fatal("unreadable state was repaired as if it were malformed JSON")
	}
	if info, err := os.Stat(GetStateFilePath()); err != nil || !info.IsDir() {
		t.Fatal("repair replaced an unreadable path")
	}
}

func TestCorruptStateRepairContinuesAfterBackupFailure(t *testing.T) {
	isolateState(t)
	if err := EnsureUpdateDir(); err != nil {
		t.Fatal(err)
	}
	broken := []byte("{unreadable-json}")
	if err := os.WriteFile(GetStateFilePath(), broken, 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	err := changeStateWithBackup(true, func(state *State) { state.AutoUpdate = true }, func(path string, _ time.Time) error {
		calls++
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, broken) {
			t.Fatal("backup was attempted after corrupt evidence was overwritten")
		}
		return errors.New("synthetic evidence-copy failure")
	})
	if err != nil || calls != 1 {
		t.Fatalf("best-effort backup blocked repair or was skipped: calls=%d err=%v", calls, err)
	}
	if state, err := LoadState(); err != nil || !state.AutoUpdate || state.Channel != "stable" {
		t.Fatalf("backup failure did not retain requested fresh defaults: %+v %v", state, err)
	}
}

func TestCorruptStateBackupUsesUTCAndDoesNotOverwriteEarlierEvidence(t *testing.T) {
	isolateState(t)
	if err := EnsureUpdateDir(); err != nil {
		t.Fatal(err)
	}
	path := GetStateFilePath()
	now := time.Date(2026, 10, 27, 8, 0, 0, 123, time.FixedZone("fixture", -4*3600))
	for _, broken := range []string{"{first}", "{second}"} {
		if err := os.WriteFile(path, []byte(broken), 0600); err != nil {
			t.Fatal(err)
		}
		if err := backupCorruptState(path, now); err != nil {
			t.Fatal(err)
		}
	}
	backups, err := filepath.Glob(path + ".corrupt-20261027T120000.000000123Z-*")
	if err != nil || len(backups) != 2 {
		t.Fatalf("backup name lost UTC or overwrote a repeated timestamp: %v %v", backups, err)
	}
	contents := map[string]bool{}
	for _, backup := range backups {
		data, err := os.ReadFile(backup)
		if err != nil {
			t.Fatal(err)
		}
		contents[string(data)] = true
	}
	if !contents["{first}"] || !contents["{second}"] {
		t.Fatalf("backup lost original bytes: %v", contents)
	}
}

func TestCorruptStateTickHonorsPolicyAndGatesRepairLog(t *testing.T) {
	for _, tc := range []struct {
		name, source    string
		enabled         bool
		notices, checks int32
	}{
		{name: "unknown preference", source: "unknown", notices: 2},
		{name: "opt out", source: "opt-out"},
		{name: "env off", source: "env"},
		{name: "dev veto", source: "dev"},
		{name: "env on", source: "env", enabled: true, notices: 2, checks: 2},
		{name: "flag on", source: "flag", enabled: true, notices: 2, checks: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateState(t)
			if err := EnsureUpdateDir(); err != nil {
				t.Fatal(err)
			}
			broken := []byte("{\"auto_update\":true}}")
			if err := os.WriteFile(GetStateFilePath(), broken, 0600); err != nil {
				t.Fatal(err)
			}
			checker := &fakeChecker{}
			ticks := make(chan time.Time)
			after := make(chan struct{}, 1)
			var lines []string
			updater := NewAutoUpdater(AutoUpdaterConfig{
				Checker: checker, Ticks: ticks, Policy: func() (bool, string) { return tc.enabled, tc.source },
				AfterTick: func() { after <- struct{}{} },
				Log:       func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) },
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() { updater.Run(ctx); close(done) }()
			for i := 0; i < 2; i++ {
				select {
				case ticks <- time.Unix(int64(i), 0):
				case <-time.After(5 * time.Second):
					t.Fatal("fixture tick was not received")
				}
				select {
				case <-after:
				case <-time.After(5 * time.Second):
					t.Fatal("corrupt tick did not finish")
				}
			}
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("fixture updater did not stop")
			}
			count := 0
			for _, line := range lines {
				if strings.Contains(line, "state file unreadable") {
					if !strings.Contains(line, "citadel update enable") || !strings.Contains(line, "citadel update disable") {
						t.Fatalf("repair commands missing: %s", line)
					}
					if tc.enabled && (!strings.Contains(line, "continuing under explicit policy") || strings.Contains(line, "paused")) {
						t.Fatalf("enabled tick incorrectly reported paused: %s", line)
					}
					if !tc.enabled && !strings.Contains(line, "paused") {
						t.Fatalf("unknown policy did not report pause: %s", line)
					}
					count++
				}
			}
			if int32(count) != tc.notices || atomic.LoadInt32(&checker.checkCalls) != tc.checks {
				t.Fatalf("corruption was silent or checked anyway: notices=%d checks=%d", count, atomic.LoadInt32(&checker.checkCalls))
			}
			if after, err := os.ReadFile(GetStateFilePath()); err != nil || !bytes.Equal(after, broken) {
				t.Fatal("paused tick changed the corrupt state")
			}
		})
	}
}

// This helper is launched only by the fixtures below, never by production code.
// TestMain still supplies a private backstop; the helper explicitly selects the
// parent fixture's private home to exercise cross-process contention.
func TestUpdateStateWriterHelper(t *testing.T) {
	home := os.Getenv("CITADEL_STATE_FIXTURE_HOME")
	if home == "" {
		return
	}
	for _, key := range []string{"HOME", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, home)
	}
	if os.Getenv("CITADEL_STATE_FIXTURE_MODE") == "mutate" {
		file, err := openStateLock(GetStateFilePath() + ".lock")
		if err != nil {
			t.Fatal(err)
		}
		locked, err := tryStateLock(file)
		if locked {
			unlockStateFile(file)
		}
		_ = file.Close()
		if err != nil || locked {
			t.Fatalf("parent transaction was not locked across processes: locked=%t err=%v", locked, err)
		}
		if err := os.WriteFile(filepath.Join(home, "child-ready"), []byte("blocked"), 0600); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 25; i++ {
			if err := incrementFixtureState(true); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	for i := 0; i < 30; i++ {
		if err := SaveState(&State{Channel: "child", AvailableUpdate: strings.Repeat("fixture", 1500+i)}); err != nil {
			t.Fatal(err)
		}
	}
}

func incrementFixtureState(enable bool) error {
	return MutateState(func(state *State) {
		count, _ := strconv.Atoi(state.AvailableUpdate)
		state.AvailableUpdate = strconv.Itoa(count + 1)
		if enable {
			state.AutoUpdate = true
		}
	})
}

func stateWriterSubprocess(t *testing.T, mode string) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestUpdateStateWriterHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "CITADEL_STATE_FIXTURE_HOME="+os.Getenv("HOME"), "CITADEL_STATE_FIXTURE_MODE="+mode)
	return cmd
}

func TestStateTransactionSerializesProcessesAndGoroutines(t *testing.T) {
	isolateState(t)
	if err := SaveState(&State{Channel: "stable", AvailableUpdate: "0"}); err != nil {
		t.Fatal(err)
	}
	cmd := stateWriterSubprocess(t, "mutate")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := withStateLock(GetStateFilePath(), func() error {
		if err := cmd.Start(); err != nil {
			return err
		}
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), "child-ready")); err == nil {
				return nil
			}
			select {
			case <-deadline.C:
				return errors.New("child did not confirm cross-process lock contention")
			case <-ticker.C:
			}
		}
	})
	if err != nil {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for writer := 0; writer < 2; writer++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if err := incrementFixtureState(false); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); errs <- cmd.Wait() }()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("fixture writer failed: %v\n%s", err, output.String())
		}
	}
	state, err := LoadState()
	if err != nil || state.AvailableUpdate != "75" || !state.AutoUpdate || state.Channel != "stable" {
		t.Fatalf("transaction lost committed changes: state=%+v err=%v", state, err)
	}
}

func TestAtomicStateSavesNeverExposePartialJSON(t *testing.T) {
	isolateState(t)
	if err := SaveState(&State{Channel: "initial"}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	readerDone := make(chan struct{})
	errs := make(chan error, 4)
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(GetStateFilePath())
			if err != nil || !json.Valid(data) {
				errs <- fmt.Errorf("reader saw a partial state: %v", err)
				return
			}
			runtime.Gosched()
		}
	}()
	cmd := stateWriterSubprocess(t, "save")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		close(stop)
		<-readerDone
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for writer := 0; writer < 2; writer++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				if err := SaveState(&State{Channel: "parent", AvailableUpdate: strings.Repeat("fixture", i)}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); errs <- cmd.Wait() }()
	wg.Wait()
	close(stop)
	<-readerDone
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent state fixture failed: %v\n%s", err, output.String())
		}
	}
	if _, err := LoadState(); err != nil {
		t.Fatal(err)
	}
}

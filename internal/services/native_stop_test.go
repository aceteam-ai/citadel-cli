// internal/services/native_stop_test.go
package services

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// helperSleepEnv turns a re-exec of this test binary into a process that does
// nothing but sleep. Re-execing the test binary (rather than /bin/sleep or a
// shell script) is what lets a test choose the process's argv[0]: the whole
// subject here is which command lines a stop is allowed to match, and a shebang
// script would appear in the process table as /bin/sh, not as the name under
// test. It exits on SIGTERM through Go's default signal disposition, exactly
// like a real engine would.
const helperSleepEnv = "CITADEL_TEST_NATIVE_SLEEP"

func TestMain(m *testing.M) {
	if os.Getenv(helperSleepEnv) != "" {
		time.Sleep(5 * time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

var fakeCounter atomic.Int64

// uniqueBinaryName returns an engine binary name that cannot collide with any
// real process on the machine running the suite. This matters: these tests kill
// processes by name, and a generic name would put a developer's actual ollama
// (or anything else) in the blast radius.
func uniqueBinaryName(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("citadel-test-engine-%d-%d", os.Getpid(), fakeCounter.Add(1))
}

// registerTestEngine installs a throwaway service in NativeServices whose port
// is closed. A closed port is the "wedged engine" shape from #649/#677: nothing
// answers, yet the process is alive and holding VRAM, and stop must still kill
// it.
func registerTestEngine(t *testing.T, svc NativeService) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("native engine stop uses unix process tooling (ps//proc), as pkill/pgrep did before it")
	}
	name := svc.Name
	if _, exists := NativeServices[name]; exists {
		t.Fatalf("test service name %q collides with a real service", name)
	}
	NativeServices[name] = svc
	t.Cleanup(func() { delete(NativeServices, name) })
	// Isolate the pidfile from the developer's real ~/.citadel-cli/run.
	t.Setenv(runDirEnv, t.TempDir())
	return name
}

// fakeProc is a spawned process with a chosen argv.
type fakeProc struct {
	pid  int
	done chan struct{}
}

// spawnFake starts a process whose command line is exactly argv and which sleeps
// until signalled. It is reaped by a background Wait so an exited helper never
// lingers as a zombie and confuses the process-table checks.
func spawnFake(t *testing.T, argv ...string) *fakeProc {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := &exec.Cmd{
		Path: exe,
		Args: argv,
		Env:  append(os.Environ(), helperSleepEnv+"=1"),
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake %v: %v", argv, err)
	}
	fp := &fakeProc{pid: cmd.Process.Pid, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(fp.done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-fp.done
	})

	// Wait until the process table actually shows the argv we asked for; exec is
	// asynchronous and a stop issued too early would find nothing to match.
	deadline := time.Now().Add(10 * time.Second)
	want := strings.Join(argv, " ")
	for time.Now().Before(deadline) {
		if line, ok := processCmdline(fp.pid); ok && strings.Contains(line, want) {
			return fp
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fake process %v never appeared in the process table", argv)
	return nil
}

func assertExited(t *testing.T, fp *fakeProc, what string) {
	t.Helper()
	select {
	case <-fp.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s (pid %d) was not stopped", what, fp.pid)
	}
}

func assertSurvived(t *testing.T, fp *fakeProc, what string) {
	t.Helper()
	// A kill would already have been delivered by the time StopNativeService
	// returned; the settle window only covers signal delivery and exit.
	select {
	case <-fp.done:
		t.Fatalf("%s (pid %d) was killed -- it is not the engine (#696)", what, fp.pid)
	case <-time.After(300 * time.Millisecond):
	}
	if _, ok := processCmdline(fp.pid); !ok {
		t.Fatalf("%s (pid %d) is gone from the process table -- it is not the engine (#696)", what, fp.pid)
	}
}

// TestProcessMatchesServiceRejectsCommandLineMentions is the decision table the
// whole fix rests on: a command line that MENTIONS the binary is not the engine.
func TestProcessMatchesServiceRejectsCommandLineMentions(t *testing.T) {
	ollama := NativeServices["ollama"]
	llamacpp := NativeServices["llamacpp"]
	vllm := NativeServices["vllm"]

	cases := []struct {
		name    string
		service NativeService
		cmdline string
		want    bool
	}{
		{"engine on PATH", ollama, "/usr/local/bin/ollama serve", true},
		{"engine bare name", ollama, "ollama serve", true},
		{"journalctl following the unit", ollama, "journalctl -u ollama -f", false},
		{"systemctl status", ollama, "systemctl status ollama", false},
		{"editor with an ollama path open", ollama, "vim /home/dev/notes/ollama.md", false},
		{"citadel stopping ollama", ollama, "/usr/local/bin/citadel service stop ollama", false},
		{"a wrapper script named after it", ollama, "/usr/bin/ollama-wrapper --serve", false},
		{"llama-server", llamacpp, "/opt/llama.cpp/llama-server --host 0.0.0.0", true},
		{"llamacpp alt binary", llamacpp, "llama-cpp-server --port 8081", true},
		{"vllm entrypoint binary", vllm, "/usr/bin/vllm serve meta-llama/Llama-3-8B", true},
		{"vllm python module form", vllm, "/usr/bin/python3 -m vllm.entrypoints.openai.api_server --model x", true},
		{"vllm python module form, versioned interpreter", vllm, "python3.11 -m vllm.entrypoints.openai.api_server", true},
		// pip installs vllm as a shebang console script, so the kernel rewrites
		// argv to `<interpreter> <script> ...`. This is how a real `vllm serve`
		// appears in the process table.
		{"vllm console script", vllm, "/usr/bin/python3 /usr/local/bin/vllm serve meta-llama/Llama-3-8B", true},
		{"python running an unrelated script", vllm, "/usr/bin/python3 /usr/local/bin/other-tool serve", false},
		{"editor opening the console script", vllm, "vim /usr/local/bin/vllm", false},
		{"pip installing it", vllm, "/usr/bin/python3 -m pip install vllm", false},
		{"python running something else", vllm, "python3 -m http.server", false},
		{"a log tail mentioning the module", vllm, "tail -f /var/log/vllm.entrypoints.openai.api_server.log", false},
		{"grep for the module", vllm, "grep -r vllm.entrypoints.openai.api_server /etc", false},
		{"empty command line", ollama, "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := processMatchesService(tc.service, tc.cmdline); got != tc.want {
				t.Errorf("processMatchesService(%q) = %v, want %v", tc.cmdline, got, tc.want)
			}
		})
	}
}

// TestStopNativeServiceFallbackStopsEngineNotLookalikes is issue #696 end to end
// for an engine citadel did not start (no pidfile): the engine must die and the
// process that merely mentions it must not.
//
// The engine's port is closed for the whole test, so this doubles as the
// #649/#677 acceptance criterion: a wedged engine that answers nothing is still
// a process holding VRAM and is still stopped.
func TestStopNativeServiceFallbackStopsEngineNotLookalikes(t *testing.T) {
	bin := uniqueBinaryName(t)
	name := registerTestEngine(t, NativeService{Name: bin, Binary: bin, Port: 1})

	engine := spawnFake(t, bin, "serve")
	bystander := spawnFake(t, "journalctl", "-u", bin, "-f")

	if IsNativeServiceServing(name) {
		t.Fatalf("test setup: the fake engine must not be answering on its port")
	}
	if err := StopNativeService(name); err != nil {
		t.Fatalf("StopNativeService: %v", err)
	}

	assertExited(t, engine, "the engine")
	assertSurvived(t, bystander, "journalctl -u <engine>")
}

// TestStopNativeServiceStopsAPythonConsoleScriptEngine is the vllm shape end to
// end. A pip-installed engine runs under the interpreter's argv0, so an
// exact-name match on argv[0] alone would leave vllm -- the engine the whole
// anchored-pattern requirement exists for -- unstoppable, while `pkill -f` did
// stop it.
func TestStopNativeServiceStopsAPythonConsoleScriptEngine(t *testing.T) {
	bin := uniqueBinaryName(t)
	name := registerTestEngine(t, NativeService{
		Name:        bin,
		Binary:      bin,
		AltBinaries: []string{"python -m " + bin + ".entrypoints.openai.api_server"},
		Port:        1,
	})

	engine := spawnFake(t, "python3", "/usr/local/bin/"+bin, "serve", "some/model")
	module := spawnFake(t, "python3", "-m", bin+".entrypoints.openai.api_server")
	editor := spawnFake(t, "vim", "/usr/local/bin/"+bin)

	if err := StopNativeService(name); err != nil {
		t.Fatalf("StopNativeService: %v", err)
	}

	assertExited(t, engine, "the pip-installed console-script engine")
	assertExited(t, module, "the python -m entrypoint form")
	assertSurvived(t, editor, "an editor with the console script open")
}

// TestStopNativeServiceSignalsOnlyTheRecordedPID pins that the pidfile actually
// TARGETS a process rather than just gating a sweep: with two identical engine
// processes alive, only the recorded one is signalled.
func TestStopNativeServiceSignalsOnlyTheRecordedPID(t *testing.T) {
	bin := uniqueBinaryName(t)
	name := registerTestEngine(t, NativeService{Name: bin, Binary: bin, Port: 1})

	ours := spawnFake(t, bin, "serve")
	theirs := spawnFake(t, bin, "serve", "--someone-elses")

	if err := writeNativePidFile(name, ours.pid); err != nil {
		t.Fatalf("writeNativePidFile: %v", err)
	}

	if err := StopNativeService(name); err != nil {
		t.Fatalf("StopNativeService: %v", err)
	}

	assertExited(t, ours, "the engine citadel started")
	assertSurvived(t, theirs, "an engine process citadel did not start")
	if _, err := os.Stat(nativePidFilePath(name)); !os.IsNotExist(err) {
		t.Errorf("pidfile should be removed after a successful stop, stat err = %v", err)
	}
}

// TestStopNativeServiceTreatsRecycledPidFileAsStale is the case that makes the
// PID approach safe: a pidfile whose PID has been recycled by an unrelated
// process must never be signalled. Killing it would be strictly worse than the
// bug being fixed.
//
// The stale record must also not block the stop: the real engine, running
// alongside, still has to be stopped through the fallback.
func TestStopNativeServiceTreatsRecycledPidFileAsStale(t *testing.T) {
	bin := uniqueBinaryName(t)
	name := registerTestEngine(t, NativeService{Name: bin, Binary: bin, Port: 1})

	// The PID was recorded for the engine, but that process died and the PID now
	// belongs to something else entirely.
	recycled := spawnFake(t, "sshd", "-D")
	engine := spawnFake(t, bin, "serve")

	if err := writeNativePidFile(name, recycled.pid); err != nil {
		t.Fatalf("writeNativePidFile: %v", err)
	}

	if err := StopNativeService(name); err != nil {
		t.Fatalf("StopNativeService: %v", err)
	}

	assertSurvived(t, recycled, "the process that recycled the recorded PID")
	assertExited(t, engine, "the engine")
	if _, err := os.Stat(nativePidFilePath(name)); !os.IsNotExist(err) {
		t.Errorf("a stale pidfile should be removed, stat err = %v", err)
	}
}

// TestStopNativeServiceIgnoresDeadPidFile covers the ordinary stale case: the
// recorded PID is simply gone. Stop must fall through to the name match instead
// of reporting success while the engine (restarted out of band under a new PID)
// keeps running.
func TestStopNativeServiceIgnoresDeadPidFile(t *testing.T) {
	bin := uniqueBinaryName(t)
	name := registerTestEngine(t, NativeService{Name: bin, Binary: bin, Port: 1})

	dead := spawnFake(t, bin, "serve")
	deadPID := dead.pid
	if err := writeNativePidFile(name, deadPID); err != nil {
		t.Fatalf("writeNativePidFile: %v", err)
	}
	if err := stopVerifiedPID(deadPID, NativeServices[name]); err != nil {
		t.Fatalf("test setup kill: %v", err)
	}
	assertExited(t, dead, "test setup process")

	engine := spawnFake(t, bin, "serve")
	if err := StopNativeService(name); err != nil {
		t.Fatalf("StopNativeService: %v", err)
	}
	assertExited(t, engine, "the engine running under a new PID")
}

// TestStartNativeServiceRecordsPIDAndStopUsesIt is the start/stop round trip:
// StartNativeService must persist the PID (callers all discard the returned
// NativeProcess, and the stop can come from a different citadel process), and a
// subsequent stop must use it without touching a lookalike.
func TestStartNativeServiceRecordsPIDAndStopUsesIt(t *testing.T) {
	bin := uniqueBinaryName(t)
	name := registerTestEngine(t, NativeService{
		Name:      bin,
		Binary:    bin,
		Port:      1,
		StartArgs: []string{"serve"},
		EnvVars:   map[string]string{helperSleepEnv: "1"},
	})

	// A symlink to this test binary stands in for the engine executable: real,
	// on PATH, and long-running.
	binDir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if err := os.Symlink(exe, filepath.Join(binDir, bin)); err != nil {
		t.Fatalf("symlink fake engine: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	proc, err := StartNativeService(name, t.TempDir())
	if err != nil {
		t.Fatalf("StartNativeService: %v", err)
	}
	started := proc.Cmd.Process.Pid
	exited := make(chan struct{})
	go func() {
		_ = proc.Cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = proc.Cmd.Process.Kill()
		<-exited
	})

	recorded, ok := readNativePidFile(name)
	if !ok {
		t.Fatalf("StartNativeService did not record a pidfile at %s", nativePidFilePath(name))
	}
	if recorded != started {
		t.Fatalf("pidfile records pid %d, engine started as %d", recorded, started)
	}

	bystander := spawnFake(t, "journalctl", "-u", bin, "-f")
	if err := StopNativeService(name); err != nil {
		t.Fatalf("StopNativeService: %v", err)
	}

	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatalf("the started engine (pid %d) was not stopped", started)
	}
	assertSurvived(t, bystander, "journalctl -u <engine>")
}

// TestStopNativeServiceUnknownService pins the guard: an unrecognised name is an
// error, never a process sweep.
func TestStopNativeServiceUnknownService(t *testing.T) {
	if err := StopNativeService("definitely-not-an-engine"); err == nil {
		t.Error("expected an error for an unknown service")
	}
}

// TestReadNativePidFileRejectsUnusableContent guards the parse: a truncated or
// garbage pidfile must not resolve to a PID at all, and never to init.
func TestReadNativePidFileRejectsUnusableContent(t *testing.T) {
	t.Setenv(runDirEnv, t.TempDir())
	for _, content := range []string{"", "\n", "not-a-pid", "0", "1", "-42"} {
		if err := os.MkdirAll(nativeRunDir(), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(nativePidFilePath("x"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		if pid, ok := readNativePidFile("x"); ok {
			t.Errorf("pidfile content %q yielded pid %d, want unusable", content, pid)
		}
	}
}

// TestNativeStopTimingDefaults pins the shipped values of the injectable timing
// vars, so a test-local override can never quietly become the product default.
func TestNativeStopTimingDefaults(t *testing.T) {
	if nativeStopGrace != 5*time.Second {
		t.Errorf("nativeStopGrace = %v, want 5s", nativeStopGrace)
	}
	if nativeStopPoll != 100*time.Millisecond {
		t.Errorf("nativeStopPoll = %v, want 100ms", nativeStopPoll)
	}
}

// TestNativeRunDirIsConfigAnchored pins where pidfiles live. Start and stop can
// run in different citadel processes, so the location must be derivable without
// an argument -- deriving it from the caller-supplied logDir would put the two
// start call sites in different directories.
func TestNativeRunDirIsConfigAnchored(t *testing.T) {
	t.Setenv(runDirEnv, "")
	if got := nativeRunDir(); filepath.Base(got) != "run" {
		t.Errorf("nativeRunDir() = %q, want a <config>/run directory", got)
	}
	if got := nativePidFilePath("ollama"); filepath.Base(got) != "ollama.pid" {
		t.Errorf("nativePidFilePath = %q, want .../ollama.pid", got)
	}

	dir := t.TempDir()
	t.Setenv(runDirEnv, dir)
	if got := nativePidFilePath("ollama"); got != filepath.Join(dir, "ollama.pid") {
		t.Errorf("%s override not honoured: %q", runDirEnv, got)
	}
}

// TestDefaultListProcessesSeesThisProcess is a smoke test for the process-table
// reader: the enumeration is what the fallback stop is built on, so a `ps`
// invocation that returns nothing (or that this parser mangles) would silently
// turn every fallback stop into a no-op.
func TestDefaultListProcessesSeesThisProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ps is unix-only")
	}
	entries, err := defaultListProcesses()
	if err != nil {
		t.Fatalf("defaultListProcesses: %v", err)
	}
	self := os.Getpid()
	for _, e := range entries {
		if e.pid == self {
			if e.cmdline == "" {
				t.Fatalf("own process listed with an empty command line")
			}
			return
		}
	}
	t.Fatalf("defaultListProcesses (%d entries) did not include this process (pid %d)", len(entries), self)
}

// TestDefaultProcessCmdlineIdentifiesThisProcess covers the verification
// primitive on both of its implementations' terms: a live PID resolves to a
// command line, an implausible one does not.
func TestDefaultProcessCmdlineIdentifiesThisProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/proc and ps are unix-only")
	}
	line, ok := defaultProcessCmdline(os.Getpid())
	if !ok || strings.TrimSpace(line) == "" {
		t.Fatalf("defaultProcessCmdline(self) = %q, %v; want a command line", line, ok)
	}
	if _, ok := defaultProcessCmdline(-1); ok {
		t.Error("defaultProcessCmdline(-1) must not report a process")
	}
	// A PID that is almost certainly unassigned. Not fatal if it happens to
	// exist; the assertion is only meaningful when it does not.
	const unlikely = 4194303
	if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(unlikely))); os.IsNotExist(err) || runtime.GOOS != "linux" {
		if _, ok := defaultProcessCmdline(unlikely); ok && runtime.GOOS == "linux" {
			t.Errorf("pid %d reported as live", unlikely)
		}
	}
}

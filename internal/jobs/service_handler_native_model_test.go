// internal/jobs/service_handler_native_model_test.go
//
// SERVICE_START model on a natively-running ollama (#543): the model contract
// for ollama is pull-based, so instead of dropping the model param ("ignored"),
// the handler must run `ollama pull <model>` — idempotent, fast when cached —
// and report FAILURE when the pull cannot be performed. These tests fake the
// ollama (and pgrep) binaries on PATH so nothing real is executed.
package jobs

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/services"
)

// serveNativeEngine makes the named native engine "already serving" for the
// duration of the test by binding a real listener and pointing its
// NativeServices entry at that port.
//
// Since #649 the already-running short-circuit asks whether the ENGINE ANSWERS,
// not whether a process name matches, so a fake `pgrep` that exits 0 no longer
// simulates a running engine -- which is the entire point of that fix. A real
// listener on an ephemeral port is used rather than the engine's true port so
// the suite never collides with an actual ollama on the developer's machine.
// deadNativeEngine is the inverse of serveNativeEngine: it points the engine at
// a port that is guaranteed closed (bound to claim it, then released).
//
// It exists so a test about a DEAD engine never depends on the engine's real
// port being free on the machine running the suite -- a developer with ollama
// listening on 11434 would otherwise see this test fail for the wrong reason,
// and CI and laptop would disagree.
func deadNativeEngine(t *testing.T, name string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	original := services.NativeServices[name]
	patched := original
	patched.Port = port
	services.NativeServices[name] = patched
	t.Cleanup(func() { services.NativeServices[name] = original })
}

func serveNativeEngine(t *testing.T, name string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	original := services.NativeServices[name]
	patched := original
	patched.Port = ln.Addr().(*net.TCPAddr).Port
	services.NativeServices[name] = patched
	t.Cleanup(func() { services.NativeServices[name] = original })
}

// fakeBinDir creates a temp dir set as the ONLY PATH entry, and returns it
// together with a helper that writes an executable shell script into it.
func fakeBinDir(t *testing.T) (string, func(name, script string)) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake-binary shell scripts are not runnable on windows")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	write := func(name, script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0755); err != nil {
			t.Fatal(err)
		}
	}
	return dir, write
}

// newNativeOllamaHandler writes a citadel.yaml declaring ollama (and llamacpp)
// as type: native so resolveKind never probes binaries.
func newNativeOllamaHandler(t *testing.T) *ServiceHandler {
	t.Helper()
	dir := t.TempDir()
	manifest := `node:
  name: test-node
services:
  - name: ollama
    type: native
  - name: llamacpp
    type: native
`
	if err := os.WriteFile(filepath.Join(dir, "citadel.yaml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	return NewServiceHandler(dir)
}

func TestEnsureOllamaModel_InvalidModel(t *testing.T) {
	// Validation happens before any exec, so no fake binary is needed.
	t.Setenv("PATH", t.TempDir())
	for _, bad := range []string{"a b", "$(rm -rf /)", "-leading", ""} {
		if err := ensureOllamaModel(JobContext{}, bad, false); err == nil {
			t.Errorf("ensureOllamaModel accepted invalid model %q", bad)
		}
	}
}

func TestEnsureOllamaModel_MissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no ollama anywhere
	err := ensureOllamaModel(JobContext{}, "qwen2.5:7b", false)
	if err == nil || !strings.Contains(err.Error(), "ollama binary not found") {
		t.Errorf("err = %v, want ollama-binary-not-found error", err)
	}
}

func TestEnsureOllamaModel_PullSuccess(t *testing.T) {
	dir, write := fakeBinDir(t)
	argsFile := filepath.Join(dir, "args.log")
	write("ollama", `echo "$@" >> `+argsFile+`
exit 0`)

	// waitForServer=true also exercises the readiness poll (`ollama list`),
	// which the fake answers immediately.
	if err := ensureOllamaModel(JobContext{}, "qwen2.5:7b", true); err != nil {
		t.Fatalf("ensureOllamaModel: %v", err)
	}
	logged, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("fake ollama was never invoked: %v", err)
	}
	if !strings.Contains(string(logged), "pull qwen2.5:7b") {
		t.Errorf("fake ollama invocations = %q, want a 'pull qwen2.5:7b' call", logged)
	}
}

func TestEnsureOllamaModel_PullFailure(t *testing.T) {
	_, write := fakeBinDir(t)
	write("ollama", `if [ "$1" = "pull" ]; then echo "pull failed: manifest not found" >&2; exit 1; fi
exit 0`)

	err := ensureOllamaModel(JobContext{}, "no-such-model", false)
	if err == nil {
		t.Fatal("expected error from failing pull, got nil")
	}
	if !strings.Contains(err.Error(), "manifest not found") {
		t.Errorf("pull failure should surface command output, got: %v", err)
	}
}

// TestServiceStartNativeOllama_PullsModel drives the full Execute path on the
// observed node-1084 shape: ollama already running natively (fake pgrep exits
// 0) and SERVICE_START carries a model. The model must be pulled — not ignored
// — and the result message must say so.
func TestServiceStartNativeOllama_PullsModel(t *testing.T) {
	dir, write := fakeBinDir(t)
	argsFile := filepath.Join(dir, "args.log")
	write("pgrep", "exit 0") // a process matches -- no longer enough on its own (#649)
	serveNativeEngine(t, "ollama")
	write("ollama", `echo "$@" >> `+argsFile+`
exit 0`)
	h := newNativeOllamaHandler(t)

	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "job-543-1",
		Type:    "SERVICE_START",
		Payload: map[string]string{"service": "ollama", "model": "qwen2.5:7b"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	logged, rErr := os.ReadFile(argsFile)
	if rErr != nil || !strings.Contains(string(logged), "pull qwen2.5:7b") {
		t.Errorf("ollama pull not invoked (log=%q, err=%v)", logged, rErr)
	}
	if !strings.Contains(string(out), "pulled") {
		t.Errorf("result should report the model as pulled, got: %s", out)
	}
	if strings.Contains(string(out), "ignored") {
		t.Errorf("result still reports the model as ignored: %s", out)
	}
}

// TestServiceStartNativeOllama_PullFailureIsJobFailure: when the pull cannot
// run (here: ollama binary absent), the job must FAIL — a hard error, not a
// soft serviceResult — so the deploy gets honest feedback (#543).
func TestServiceStartNativeOllama_PullFailureIsJobFailure(t *testing.T) {
	_, write := fakeBinDir(t)
	write("pgrep", "exit 0") // running, but no ollama binary on PATH
	serveNativeEngine(t, "ollama")
	h := newNativeOllamaHandler(t)

	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "job-543-2",
		Type:    "SERVICE_START",
		Payload: map[string]string{"service": "ollama", "model": "qwen2.5:7b"},
	})
	if err == nil || !strings.Contains(err.Error(), "ollama binary not found") {
		t.Errorf("err = %v, want hard ollama-binary-not-found failure", err)
	}
}

// TestServiceStartNativeNonOllama_ModelStillIgnored pins the unchanged
// behavior for native engines with no pull mechanism: the model is logged as
// ignored and the start succeeds without any pull attempt.
func TestServiceStartNativeNonOllama_ModelStillIgnored(t *testing.T) {
	_, write := fakeBinDir(t)
	write("pgrep", "exit 0") // llamacpp process matches
	serveNativeEngine(t, "llamacpp")
	h := newNativeOllamaHandler(t)

	var logs []string
	ctx := JobContext{LogFn: func(_, msg string) { logs = append(logs, msg) }}
	out, err := h.Execute(ctx, &nexus.Job{
		ID:      "job-543-3",
		Type:    "SERVICE_START",
		Payload: map[string]string{"service": "llamacpp", "model": "some/model"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(string(out), "already running") {
		t.Errorf("result = %s, want already-running", out)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "ignored") {
		t.Errorf("expected 'ignored' log for non-ollama native engine, logs: %v", logs)
	}
}

// TestServiceStartNativeOllama_DeadEngineIsStartedNotShortCircuited is the
// handler-level regression test for #649.
//
// On node 1297 a `pgrep -f ollama` match made SERVICE_START report "already
// running" while the engine was dead and the API answered nothing; the platform
// kept advertising the node as serving and every routed request timed out. The
// short-circuit must now key on the engine ANSWERING, so a process match with a
// dead port has to fall through and actually start it.
//
// deadNativeEngine pins a closed port rather than relying on the engine's real
// port being free, so the result does not depend on whether the machine running
// the suite happens to have ollama up. Asserting that `ollama serve` was invoked (rather
// than just checking the message) is what distinguishes a real start from a
// reworded short-circuit.
func TestServiceStartNativeOllama_DeadEngineIsStartedNotShortCircuited(t *testing.T) {
	dir, write := fakeBinDir(t)
	argsFile := filepath.Join(dir, "args.log")
	write("pgrep", "exit 0")      // a matching process exists...
	deadNativeEngine(t, "ollama") // ...but nothing answers on its port
	write("ollama", `echo "$@" >> `+argsFile+`
exit 0`)
	h := newNativeOllamaHandler(t)

	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "job-649-1",
		Type:    "SERVICE_START",
		Payload: map[string]string{"service": "ollama"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Assert on WHICH BRANCH the handler took, via the result message, rather
	// than on the fake binary's side effects. StartNativeService uses cmd.Start(),
	// so the child runs concurrently and any file it writes is a race the test
	// would lose under load -- observed failing only inside a full `go test ./...`
	// while passing for the package alone. The branch is the actual subject here:
	// "already running" is the #649 bug, "started" is the fix.
	if strings.Contains(string(out), "already running") {
		t.Errorf("dead engine was short-circuited as already running (#649): %s", out)
	}
	if !strings.Contains(string(out), "started") {
		t.Errorf("expected the handler to start the dead engine, got: %s", out)
	}
}

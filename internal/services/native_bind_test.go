// internal/services/native_bind_test.go
package services

import "testing"

// hostArg walks a StartArgs slice and returns the value following the first
// "--host" token. `ok` is false when no "--host" flag is present at all -- which
// for an engine whose own default is all-interfaces (llama-server, vllm) is a
// FAILURE, not a pass, so callers must treat a missing flag as "not loopback".
func hostArg(args []string) (value string, ok bool) {
	for i, a := range args {
		if a == "--host" && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// TestNativeLlamaCppBindsLoopback pins the #1024 fix: the native (no-Docker)
// llama-server start must bind 127.0.0.1, never 0.0.0.0. A raw process has no
// docker-proxy in front of it, so an all-interfaces bind is the same
// unauthenticated-LLM-on-the-LAN exposure aceteam#9523 (PR #1025) closed for the
// compose path. This fails against the old `--host 0.0.0.0` value.
func TestNativeLlamaCppBindsLoopback(t *testing.T) {
	svc, ok := NativeServices["llamacpp"]
	if !ok {
		t.Fatal("NativeServices[\"llamacpp\"] must exist")
	}
	host, ok := hostArg(svc.StartArgs)
	if !ok {
		t.Fatalf("llamacpp StartArgs must set --host explicitly (llama-server defaults to all interfaces); got %v", svc.StartArgs)
	}
	if host != "127.0.0.1" {
		t.Errorf("llamacpp native --host = %q, want 127.0.0.1 (#1024): a native process has no docker-proxy, so an all-interfaces bind exposes the unauthenticated engine on the LAN", host)
	}
}

// TestNativeServicesBindSweep is the native mirror of #1025's
// TestServiceMapBindSweep: no NativeServices entry may hardcode an
// all-interfaces listen host in its StartArgs. This is what makes "find every
// entry that binds 0.0.0.0" (issue #1024, step 1) durable -- a future engine
// added with `--host 0.0.0.0` trips this test instead of silently shipping.
//
// ollama ("serve") and vllm ("serve") set no --host, so they do not appear
// here: ollama with no OLLAMA_HOST defaults to loopback (127.0.0.1:11434) and
// has no all-interfaces bind to fix, and vllm's StartArgs is a non-functional
// placeholder (no model arg) -- vLLM's own default IS all-interfaces, so a
// future functional native vllm entry must carry an explicit --host 127.0.0.1
// and would then be covered by this same sweep.
func TestNativeServicesBindSweep(t *testing.T) {
	for name, svc := range NativeServices {
		for _, arg := range svc.StartArgs {
			if arg == "0.0.0.0" || arg == "::" {
				t.Errorf("NativeServices[%q] StartArgs bind to all interfaces (%q); must be loopback (#1024)", name, arg)
			}
		}
	}
}

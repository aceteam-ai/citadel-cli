// internal/services/native_serving_test.go
package services

import (
	"net"
	"testing"
	"time"
)

// listenOnFreePort opens a real listener and returns its port. The tests use a
// live socket rather than a mock because the whole point of the #649 fix is that
// the predicate consults the network instead of the process table -- a mocked
// dialer would test the mock.
func listenOnFreePort(t *testing.T) (port int, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln.Addr().(*net.TCPAddr).Port, func() { _ = ln.Close() }
}

// TestPortAnswersDistinguishesLiveFromDeadPort is the core of #649: the fix is
// only worth anything if the probe actually changes answer when the socket goes
// away. Asserting only the live case would pass against a stub that always
// returns true.
func TestPortAnswersDistinguishesLiveFromDeadPort(t *testing.T) {
	port, closeFn := listenOnFreePort(t)
	if !portAnswers(port, time.Second) {
		t.Fatalf("expected port %d to answer while a listener is open", port)
	}

	closeFn()
	if portAnswers(port, 250*time.Millisecond) {
		t.Errorf("expected port %d NOT to answer after the listener closed", port)
	}
}

// TestPortAnswersRejectsUnsetPort covers the fail-closed path for a service with
// no configured port. Returning true there would reinstate the bug for any
// future engine added to NativeServices without a port.
func TestPortAnswersRejectsUnsetPort(t *testing.T) {
	if portAnswers(0, time.Second) {
		t.Error("port 0 must never be reported as answering")
	}
	if portAnswers(-1, time.Second) {
		t.Error("a negative port must never be reported as answering")
	}
}

// TestIsNativeServiceServingUnknownServiceFailsClosed pins that an unrecognised
// service name is not serving. It shares this with IsNativeServiceRunning, but
// the two are separate functions now and a copy-paste that forgot the guard
// would dial port 0 forever.
func TestIsNativeServiceServingUnknownServiceFailsClosed(t *testing.T) {
	if IsNativeServiceServing("definitely-not-an-engine") {
		t.Error("an unknown service must never be reported as serving")
	}
}

// TestIsNativeServiceServingFollowsThePort is the end-to-end shape of the bug on
// node 1297: ollama's port is dead, so ollama is not serving -- regardless of
// what the process table says. The suite cannot bind 11434 (it may be in use, and
// tests must not fight a real engine), so this asserts the negative direction,
// which is the one the fix exists for. The positive direction is covered by
// TestPortAnswersDistinguishesLiveFromDeadPort against the same helper.
func TestIsNativeServiceServingFollowsThePort(t *testing.T) {
	const name = "probe-test-engine"
	port, closeFn := listenOnFreePort(t)
	closeFn() // nothing is listening now

	original, had := NativeServices[name]
	NativeServices[name] = NativeService{Name: name, Binary: "sh", Port: port}
	t.Cleanup(func() {
		if had {
			NativeServices[name] = original
		} else {
			delete(NativeServices, name)
		}
	})

	// "sh" is chosen as the binary precisely because `pgrep -f sh` matches
	// something on essentially any machine -- the loose process check that caused
	// #649. The serving probe must disagree with it.
	if IsNativeServiceServing(name) {
		t.Error("engine must not be reported as serving when its port is dead")
	}
}

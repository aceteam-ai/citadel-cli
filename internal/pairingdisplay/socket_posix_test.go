//go:build !windows

package pairingdisplay

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// requestTimeout bounds every RequestPendingCode call in this file. Real
// dials against a local unix socket resolve near-instantly; this only
// guards against a genuinely hung test.
const requestTimeout = 2 * time.Second

func TestRequestPendingCode_NoWorkerRunning(t *testing.T) {
	dir := t.TempDir() // no socket file ever created here

	info, err := RequestPendingCode(dir, requestTimeout)
	if err != nil {
		t.Fatalf("expected no error when nothing is listening, got %v", err)
	}
	if info.Pending {
		t.Fatalf("expected Pending=false, got %+v", info)
	}
}

func TestRequestPendingCode_EmptyStateDir(t *testing.T) {
	info, err := RequestPendingCode("", requestTimeout)
	if err != nil {
		t.Fatalf("expected no error for an empty state dir, got %v", err)
	}
	if info.Pending {
		t.Fatalf("expected Pending=false, got %+v", info)
	}
}

func TestRequestPendingCode_NothingPendingOnLiveManager(t *testing.T) {
	dir := t.TempDir()
	r := newFakeRenderer()
	m := NewManager(r)
	m.stateDir = dir
	// No Show call: no socket is listening at all, same as "no worker
	// running" from the client's point of view.
	t.Cleanup(m.Shutdown)

	info, err := RequestPendingCode(dir, requestTimeout)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if info.Pending {
		t.Fatalf("expected Pending=false, got %+v", info)
	}
}

func TestRequestPendingCode_ServesRealPendingCode(t *testing.T) {
	// This is the load-bearing proof for P1: the pull command actually
	// observes a DIFFERENT process's (here, a different Manager instance
	// sharing the same on-disk state dir, standing in for "the long-running
	// citadel work process") pending code over the socket -- not a fresh,
	// empty in-process singleton. See the PR description's "read mechanism"
	// section for why this distinction is the whole point of P1.
	dir := t.TempDir()
	r := newFakeRenderer()
	// Console rendering unavailable (the realistic P1 target: a headless
	// node) -- the pull command must still work.
	r.resolveOK = false
	r.resolveReason = "no_console"
	m := NewManager(r)
	m.stateDir = dir
	t.Cleanup(m.Shutdown)

	out := m.Show("87654321", time.Hour, "gr_pull_1", "Agent Ops for jane@example.com")
	if out.Delivered {
		t.Fatalf("expected console delivery to fail (no_console), got %+v", out)
	}

	// A brand-new client, querying only via the socket path -- it never
	// touches m or the Get() singleton.
	info, err := RequestPendingCode(dir, requestTimeout)
	if err != nil {
		t.Fatalf("RequestPendingCode: %v", err)
	}
	if !info.Pending {
		t.Fatalf("expected a pending code despite no_console, got %+v", info)
	}
	if info.Code != "87654321" {
		t.Fatalf("expected the real code, got %q", info.Code)
	}
	if info.GrantRequestID != "gr_pull_1" {
		t.Fatalf("expected grant_request_id gr_pull_1, got %q", info.GrantRequestID)
	}
	if info.RequestedBy != "Agent Ops for jane@example.com" {
		t.Fatalf("expected requested_by preserved, got %q", info.RequestedBy)
	}
	if info.TTLSeconds <= 0 {
		t.Fatalf("expected a positive remaining TTL, got %d", info.TTLSeconds)
	}
	if !info.ExpiresAt.After(time.Now()) {
		t.Fatalf("expected expires_at in the future, got %v", info.ExpiresAt)
	}
}

func TestRequestPendingCode_ServesRealPendingCodeWhenConsoleDelivered(t *testing.T) {
	// The pull command must also work on a headed node that DID render to
	// its console -- the socket does not become unavailable just because
	// the console leg succeeded.
	dir := t.TempDir()
	r := newFakeRenderer()
	m := NewManager(r)
	m.stateDir = dir
	t.Cleanup(m.Shutdown)

	out := m.Show("11112222", time.Hour, "gr_pull_2", "")
	if !out.Delivered {
		t.Fatalf("expected console delivery to succeed, got %+v", out)
	}

	info, err := RequestPendingCode(dir, requestTimeout)
	if err != nil {
		t.Fatalf("RequestPendingCode: %v", err)
	}
	if !info.Pending || info.Code != "11112222" {
		t.Fatalf("expected the pending code to still be servable, got %+v", info)
	}
}

func TestRequestPendingCode_UnavailableAfterClear(t *testing.T) {
	dir := t.TempDir()
	r := newFakeRenderer()
	m := NewManager(r)
	m.stateDir = dir
	t.Cleanup(m.Shutdown)

	m.Show("12345678", time.Hour, "gr_1", "")
	if info, err := RequestPendingCode(dir, requestTimeout); err != nil || !info.Pending {
		t.Fatalf("expected a pending code before Clear, got info=%+v err=%v", info, err)
	}

	m.Clear("gr_1")

	info, err := RequestPendingCode(dir, requestTimeout)
	if err != nil {
		t.Fatalf("expected no error after Clear (socket simply stops listening), got %v", err)
	}
	if info.Pending {
		t.Fatalf("expected Pending=false after Clear, got %+v", info)
	}
}

func TestRequestPendingCode_UnavailableAfterReplacement(t *testing.T) {
	dir := t.TempDir()
	r := newFakeRenderer()
	m := NewManager(r)
	m.stateDir = dir
	t.Cleanup(m.Shutdown)

	m.Show("11111111", time.Hour, "gr_1", "")
	m.Show("22222222", time.Hour, "gr_2", "")

	info, err := RequestPendingCode(dir, requestTimeout)
	if err != nil {
		t.Fatalf("RequestPendingCode: %v", err)
	}
	if !info.Pending || info.Code != "22222222" || info.GrantRequestID != "gr_2" {
		t.Fatalf("expected only the latest (gr_2) code to be servable, got %+v", info)
	}
}

func TestRequestPendingCode_UnavailableAfterTTLExpiry(t *testing.T) {
	dir := t.TempDir()
	r := newFakeRenderer()
	m := NewManager(r)
	m.stateDir = dir
	t.Cleanup(m.Shutdown)

	m.Show("12345678", 20*time.Millisecond, "gr_1", "")

	deadline := time.Now().Add(2 * time.Second)
	for r.clearCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if r.clearCount() == 0 {
		t.Fatalf("expected TTL expiry to have fired")
	}

	info, err := RequestPendingCode(dir, requestTimeout)
	if err != nil {
		t.Fatalf("expected no error for an expired/cleared code, got %v", err)
	}
	if info.Pending {
		t.Fatalf("expected an expired code to never be served, got %+v", info)
	}
}

func TestRequestPendingCode_UnavailableAfterShutdown(t *testing.T) {
	dir := t.TempDir()
	r := newFakeRenderer()
	m := NewManager(r)
	m.stateDir = dir

	m.Show("12345678", time.Hour, "gr_1", "")
	m.Shutdown()

	info, err := RequestPendingCode(dir, requestTimeout)
	if err != nil {
		t.Fatalf("expected no error after Shutdown, got %v", err)
	}
	if info.Pending {
		t.Fatalf("expected Pending=false after Shutdown, got %+v", info)
	}
	if _, err := os.Stat(filepath.Join(dir, socketFileName)); !os.IsNotExist(err) {
		t.Fatalf("expected the socket file removed after Shutdown, stat err=%v", err)
	}
}

func TestManager_SocketFileModeIsOwnerOnly(t *testing.T) {
	// POSIX only: the 0600 permission bit is a real, kernel-enforced
	// access-control boundary here (see socket.go's package doc). On
	// Windows this file is not even built -- socket_windows.go never
	// creates a socket file at all, so there is no equivalent assertion to
	// make there (a synthesized-from-read-only-attribute mode would be a
	// false invariant, which is exactly what motivated splitting this file
	// out of the platform-independent test file).
	dir := t.TempDir()
	r := newFakeRenderer()
	m := NewManager(r)
	m.stateDir = dir
	t.Cleanup(m.Shutdown)

	m.Show("12345678", time.Hour, "gr_1", "")

	fi, err := os.Stat(filepath.Join(dir, socketFileName))
	if err != nil {
		t.Fatalf("expected the socket file to exist: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("expected socket file mode 0600, got %#o", perm)
	}
}

func TestManager_SentinelNeverLeaksThroughSocketAfterClear(t *testing.T) {
	// The socket-serving half of the §10.3 no-leak discipline: once a code
	// is cleared/expired/replaced, nothing -- not even a client that already
	// knows the socket path -- can read it back.
	const sentinel = "SENTINEL-SOCKET-9f3ac21b"
	dir := t.TempDir()
	r := newFakeRenderer()
	m := NewManager(r)
	m.stateDir = dir
	t.Cleanup(m.Shutdown)

	m.Show(sentinel, 20*time.Millisecond, "gr_1", "")
	if info, err := RequestPendingCode(dir, requestTimeout); err != nil || info.Code != sentinel {
		t.Fatalf("expected the socket to serve the sentinel while pending, got info=%+v err=%v", info, err)
	}

	m.Clear("gr_1")
	info, err := RequestPendingCode(dir, requestTimeout)
	if err != nil {
		t.Fatalf("RequestPendingCode after clear: %v", err)
	}
	if info.Pending || info.Code != "" {
		t.Fatalf("SECURITY: sentinel still servable after Clear: %+v", info)
	}
}

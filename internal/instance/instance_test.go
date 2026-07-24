//go:build !windows

package instance

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakePTY is a ptyIO whose Read blocks until released, so relaySession's
// teardown can be tested deterministically without a real PTY/shell. Releasing
// it (or closing the conn) simulates the shell exiting.
type fakePTY struct {
	mu        sync.Mutex
	writes    [][]byte
	resizedTo [2]uint16
	release   chan struct{} // Read returns io.EOF once closed (shell "exits")
}

func newFakePTY() *fakePTY { return &fakePTY{release: make(chan struct{})} }

func (f *fakePTY) Read(p []byte) (int, error) {
	<-f.release
	return 0, io.EOF
}

func (f *fakePTY) exit() { close(f.release) }

func (f *fakePTY) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, append([]byte(nil), p...))
	return len(p), nil
}

func (f *fakePTY) Resize(cols, rows uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizedTo = [2]uint16{cols, rows}
	return nil
}

// TestRelaySession_ShellExitClosesConn is the regression test for the reported
// freeze: when the shell exits (session.Read returns EOF), relaySession must
// close the conn so BOTH the relay loop and the attached client detach, instead
// of hanging on their respective conn.Read.
func TestRelaySession_ShellExitClosesConn(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	session := newFakePTY()

	done := make(chan struct{})
	go func() {
		relaySession(serverConn, session)
		close(done)
	}()

	session.exit() // the shell exits

	// The attached client must observe the connection close (EOF), not hang.
	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	if _, err := clientConn.Read(buf); err == nil {
		t.Fatal("client Read returned nil error after shell exit; expected the conn to be closed")
	}

	// relaySession itself must return (its client->PTY loop unblocked by the close).
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relaySession did not return after shell exit — teardown hung (the freeze regression)")
	}
	_ = clientConn.Close()
}

// TestRelaySession_ResizeMessage verifies a 5-byte resize frame is applied to
// the session and not forwarded as shell input.
func TestRelaySession_ResizeMessage(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	session := newFakePTY() // Read blocks (shell alive) so the relay stays up
	defer session.exit()    // release the PTY->client goroutine at the end

	done := make(chan struct{})
	go func() {
		relaySession(serverConn, session)
		close(done)
	}()

	// Send a resize frame: 0x00, cols=120 (LE), rows=40 (LE).
	frame := []byte{0x00, 120, 0, 40, 0}
	_ = clientConn.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := clientConn.Write(frame); err != nil {
		t.Fatalf("write resize frame: %v", err)
	}
	_ = clientConn.Close() // ends the relay's client->PTY loop
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relaySession did not return after client close")
	}

	session.mu.Lock()
	got := session.resizedTo
	sawWrite := len(session.writes)
	session.mu.Unlock()
	if got != [2]uint16{120, 40} {
		t.Errorf("resize not applied: got %v, want [120 40]", got)
	}
	if sawWrite != 0 {
		t.Errorf("resize frame must not be written to the shell as input, got %d writes", sawWrite)
	}
}

func TestListenAndIsRunning(t *testing.T) {
	dir := t.TempDir()

	// No instance running yet
	if IsRunning(dir) {
		t.Fatal("expected no instance running")
	}

	// Start server
	srv, err := Listen(dir)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if srv == nil {
		t.Fatal("expected non-nil server")
	}
	defer srv.Close()

	// Now it should be running
	if !IsRunning(dir) {
		t.Fatal("expected instance running after Listen")
	}

	// Second Listen should return nil (another instance detected)
	srv2, err := Listen(dir)
	if err != nil {
		t.Fatalf("second Listen: %v", err)
	}
	if srv2 != nil {
		srv2.Close()
		t.Fatal("expected nil server from second Listen")
	}
}

func TestStaleSocketCleanup(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, socketName)

	// Create a stale socket file (not listening)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("create stale socket: %v", err)
	}
	ln.Close() // immediately close — socket file remains

	// Listen should clean up the stale socket and succeed
	srv, err := Listen(dir)
	if err != nil {
		t.Fatalf("Listen after stale: %v", err)
	}
	if srv == nil {
		t.Fatal("expected server after stale cleanup")
	}
	defer srv.Close()

	if !IsRunning(dir) {
		t.Fatal("expected instance running after stale cleanup")
	}
}

func TestPIDFile(t *testing.T) {
	dir := t.TempDir()

	// No PID file → 0
	if pid := PID(dir); pid != 0 {
		t.Fatalf("expected PID 0, got %d", pid)
	}

	// Write PID
	if err := WritePID(dir); err != nil {
		t.Fatalf("WritePID: %v", err)
	}

	pid := PID(dir)
	if pid != os.Getpid() {
		t.Fatalf("expected PID %d, got %d", os.Getpid(), pid)
	}

	// Remove PID
	RemovePID(dir)
	if pid := PID(dir); pid != 0 {
		t.Fatalf("expected PID 0 after remove, got %d", pid)
	}
}

func TestServerClose(t *testing.T) {
	dir := t.TempDir()

	srv, err := Listen(dir)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if srv == nil {
		t.Fatal("expected server")
	}

	srv.Close()

	// Socket should be removed
	sockPath := filepath.Join(dir, socketName)
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Fatal("expected socket removed after Close")
	}

	// IsRunning should return false
	if IsRunning(dir) {
		t.Fatal("expected not running after Close")
	}
}

func TestSocketPermissions(t *testing.T) {
	dir := t.TempDir()

	srv, err := Listen(dir)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer srv.Close()

	sockPath := filepath.Join(dir, socketName)
	info, err := os.Stat(sockPath)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}

	perm := info.Mode().Perm()
	if perm&0077 != 0 {
		t.Errorf("socket permissions too open: %o (expected 0600)", perm)
	}
}

func TestClientConnect(t *testing.T) {
	dir := t.TempDir()

	srv, err := Listen(dir)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer srv.Close()

	// Connect as a client
	sockPath := SocketPath(dir)
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Give server time to accept and spawn PTY
	time.Sleep(200 * time.Millisecond)

	// The server should have spawned a PTY — write a command and read output
	_, err = conn.Write([]byte("echo hello-test\r"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	// Read some output (PTY should echo back and show prompt)
	buf := make([]byte, 4096)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n == 0 {
		t.Fatal("expected some PTY output")
	}

	conn.Close()
}

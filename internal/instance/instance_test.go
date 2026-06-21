//go:build !windows

package instance

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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

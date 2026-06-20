// internal/console/pty_test.go
//go:build !windows

package console

import (
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewPTYSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY not supported on Windows")
	}

	session, err := NewPTYSession(PTYConfig{
		Shell:       "/bin/sh",
		InitialCols: 80,
		InitialRows: 24,
	})
	if err != nil {
		t.Fatalf("NewPTYSession failed: %v", err)
	}
	defer session.Close()

	if session.IsClosed() {
		t.Fatal("session should not be closed after creation")
	}

	cols, rows := session.Size()
	if cols != 80 || rows != 24 {
		t.Fatalf("expected 80x24, got %dx%d", cols, rows)
	}
}

func TestPTYSessionDefaultSize(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY not supported on Windows")
	}

	session, err := NewPTYSession(PTYConfig{Shell: "/bin/sh"})
	if err != nil {
		t.Fatalf("NewPTYSession failed: %v", err)
	}
	defer session.Close()

	cols, rows := session.Size()
	if cols != 80 || rows != 24 {
		t.Fatalf("default size should be 80x24, got %dx%d", cols, rows)
	}
}

func TestPTYSessionReadWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY not supported on Windows")
	}

	session, err := NewPTYSession(PTYConfig{Shell: "/bin/sh"})
	if err != nil {
		t.Fatalf("NewPTYSession failed: %v", err)
	}
	defer session.Close()

	// Write a command
	marker := "HELLO_CONSOLE_TEST_12345"
	_, err = session.Write([]byte("echo " + marker + "\n"))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Read output until we find the marker or timeout
	buf := make([]byte, 4096)
	var collected strings.Builder
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for echo output; got so far: %s", collected.String())
		default:
		}
		n, err := session.Read(buf)
		if n > 0 {
			collected.Write(buf[:n])
			if strings.Contains(collected.String(), marker) {
				return // success
			}
		}
		if err != nil {
			t.Fatalf("Read error: %v; collected: %s", err, collected.String())
		}
	}
}

func TestPTYSessionResize(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY not supported on Windows")
	}

	session, err := NewPTYSession(PTYConfig{Shell: "/bin/sh"})
	if err != nil {
		t.Fatalf("NewPTYSession failed: %v", err)
	}
	defer session.Close()

	err = session.Resize(120, 40)
	if err != nil {
		t.Fatalf("Resize failed: %v", err)
	}

	cols, rows := session.Size()
	if cols != 120 || rows != 40 {
		t.Fatalf("expected 120x40 after resize, got %dx%d", cols, rows)
	}
}

func TestPTYSessionResizeInvalid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY not supported on Windows")
	}

	session, err := NewPTYSession(PTYConfig{Shell: "/bin/sh"})
	if err != nil {
		t.Fatalf("NewPTYSession failed: %v", err)
	}
	defer session.Close()

	err = session.Resize(0, 24)
	if err == nil {
		t.Fatal("expected error for zero cols")
	}

	err = session.Resize(80, 0)
	if err == nil {
		t.Fatal("expected error for zero rows")
	}
}

func TestPTYSessionClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY not supported on Windows")
	}

	closeCalled := false
	var closeOnce sync.Mutex
	session, err := NewPTYSession(PTYConfig{
		Shell: "/bin/sh",
		OnClose: func() {
			closeOnce.Lock()
			closeCalled = true
			closeOnce.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("NewPTYSession failed: %v", err)
	}

	err = session.Close()
	if err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if !session.IsClosed() {
		t.Fatal("session should be closed after Close()")
	}

	closeOnce.Lock()
	if !closeCalled {
		t.Fatal("OnClose callback was not called")
	}
	closeOnce.Unlock()

	// Double close should be safe
	err = session.Close()
	if err != nil {
		t.Fatalf("second Close should be no-op, got: %v", err)
	}
}

func TestPTYSessionInvalidShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY not supported on Windows")
	}

	_, err := NewPTYSession(PTYConfig{Shell: "/nonexistent/shell"})
	if err == nil {
		t.Fatal("expected error for nonexistent shell")
	}
}

func TestPTYSessionWriteAfterClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY not supported on Windows")
	}

	session, err := NewPTYSession(PTYConfig{Shell: "/bin/sh"})
	if err != nil {
		t.Fatalf("NewPTYSession failed: %v", err)
	}

	_ = session.Close()

	_, err = session.Write([]byte("hello"))
	if err == nil {
		t.Fatal("expected error writing to closed session")
	}
}

func TestPTYSessionResizeAfterClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY not supported on Windows")
	}

	session, err := NewPTYSession(PTYConfig{Shell: "/bin/sh"})
	if err != nil {
		t.Fatalf("NewPTYSession failed: %v", err)
	}

	_ = session.Close()

	err = session.Resize(100, 50)
	if err == nil {
		t.Fatal("expected error resizing closed session")
	}
}

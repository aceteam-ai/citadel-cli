package desktop

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestCheckVNCReady_Success(t *testing.T) {
	// Start a TCP listener to simulate a VNC server
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer ln.Close()

	// Accept connections in the background
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	err = CheckVNCReady(ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Errorf("expected nil error, got: %v", err)
	}
}

func TestCheckVNCReady_Failure(t *testing.T) {
	// Use a port that nothing is listening on
	err := CheckVNCReady("127.0.0.1:59999", 1*time.Second)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var re *VNCReadinessError
	if !errors.As(err, &re) {
		t.Fatalf("expected *VNCReadinessError, got %T", err)
	}
	if re.Reason != ReasonDialFailed {
		t.Errorf("reason = %q, want %q", re.Reason, ReasonDialFailed)
	}
}

func TestWaitForVNCReady_AlreadyReady(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	err = WaitForVNCReady(context.Background(), ln.Addr().String(), 3*time.Second)
	if err != nil {
		t.Errorf("expected nil error, got: %v", err)
	}
}

func TestWaitForVNCReady_BecomesReady(t *testing.T) {
	// Allocate a port but don't listen yet
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() // Close so port is free but nothing is listening

	// Start listening after a short delay
	go func() {
		time.Sleep(400 * time.Millisecond)
		newLn, err := net.Listen("tcp", addr)
		if err != nil {
			return
		}
		defer newLn.Close()
		for {
			conn, err := newLn.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	err = WaitForVNCReady(context.Background(), addr, 5*time.Second)
	if err != nil {
		t.Errorf("expected nil error (server became ready), got: %v", err)
	}
}

func TestWaitForVNCReady_Timeout(t *testing.T) {
	// Use a port that nothing will ever listen on
	err := WaitForVNCReady(context.Background(), "127.0.0.1:59998", 1*time.Second)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}

	var re *VNCReadinessError
	if !errors.As(err, &re) {
		t.Fatalf("expected *VNCReadinessError, got %T", err)
	}
	if re.Reason != ReasonPortNotOpen {
		t.Errorf("reason = %q, want %q", re.Reason, ReasonPortNotOpen)
	}
}

func TestWaitForVNCReady_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a short delay
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	err := WaitForVNCReady(ctx, "127.0.0.1:59997", 10*time.Second)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestVNCReadinessError_ErrorString(t *testing.T) {
	e := &VNCReadinessError{
		Reason: ReasonNoDisplay,
		Detail: "no DISPLAY set",
	}
	got := e.Error()
	want := "VNC not ready: no_display (no DISPLAY set)"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestVNCReadinessError_Unwrap(t *testing.T) {
	e := &VNCReadinessError{
		Reason: ReasonDialFailed,
		Detail: "connection refused",
	}
	// Should satisfy errors.As
	var re *VNCReadinessError
	if !errors.As(e, &re) {
		t.Error("errors.As failed for *VNCReadinessError")
	}
}

func TestDiagnoseDesktopReadiness_Reasons(t *testing.T) {
	// We can't fully test platform-specific diagnosis in unit tests,
	// but we can verify the error type contract.
	err := DiagnoseDesktopReadiness()
	if err != nil {
		var re *VNCReadinessError
		if !errors.As(err, &re) {
			t.Errorf("DiagnoseDesktopReadiness() returned non-VNCReadinessError: %T", err)
		}
		// The reason should be one of the known constants
		validReasons := map[string]bool{
			ReasonNoDisplay:     true,
			ReasonNoScreenTools: true,
			ReasonPlatformUnsup: true,
		}
		if !validReasons[re.Reason] {
			t.Errorf("unexpected reason %q", re.Reason)
		}
		// Detail should be non-empty
		if re.Detail == "" {
			t.Error("Detail should not be empty")
		}
	}
}

func TestCheckVNCReady_ErrorDetail(t *testing.T) {
	err := CheckVNCReady("127.0.0.1:59996", 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected error")
	}
	var re *VNCReadinessError
	if !errors.As(err, &re) {
		t.Fatalf("expected *VNCReadinessError, got %T", err)
	}
	// Detail should contain the address
	if re.Detail == "" {
		t.Error("Detail should not be empty")
	}
	// Should mention the address in the detail
	wantAddr := "127.0.0.1:59996"
	if got := re.Detail; len(got) > 0 && !containsStr(got, wantAddr) {
		t.Errorf("Detail %q should mention address %q", got, wantAddr)
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && fmt.Sprintf("%s", s) != "" && searchStr(s, substr)
}

func searchStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

//go:build windows

package pairingdisplay

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRequestPendingCode_WindowsIsHonestlyUnsupported pins the review-
// mandated contract for citadel #659 P1's Windows posture: RequestPendingCode
// must fail with a DISTINCT, non-nil error rather than silently returning
// Pending:false. Conflating "this platform doesn't support the pull
// command" with "nothing is pending" would tell an operator there is no
// code to retrieve when in fact there might be one they simply can't reach
// this way -- see socket_windows.go's doc comment.
func TestRequestPendingCode_WindowsIsHonestlyUnsupported(t *testing.T) {
	info, err := RequestPendingCode(t.TempDir(), 2*time.Second)
	if err == nil {
		t.Fatalf("expected a non-nil error on Windows, got nil (info=%+v)", info)
	}
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("expected ErrUnsupportedPlatform, got %v", err)
	}
	if info.Pending {
		t.Fatalf("expected Pending=false alongside the error, got %+v", info)
	}
}

func TestRequestPendingCode_WindowsUnsupportedEvenWithEmptyStateDir(t *testing.T) {
	// Unlike the POSIX implementation (which short-circuits an empty
	// stateDir to Pending:false, nil), the Windows stub returns the SAME
	// honest error regardless of stateDir -- there is no code path here
	// that could ever succeed on this platform.
	_, err := RequestPendingCode("", 2*time.Second)
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("expected ErrUnsupportedPlatform even for an empty stateDir, got %v", err)
	}
}

// TestManager_SocketNoOpsOnWindows pins that Show/Clear/Shutdown never
// attempt to open a socket on Windows: startSocketLocked/stopSocketLocked
// are no-ops, so a pending code's cross-process-tracking lifecycle
// (independent of console-render success, per manager.go's Show) still
// works internally, it just has no pull-command surface -- no socket file
// is ever created, and no permission model is exposed that Windows cannot
// actually enforce (see socket_windows.go's doc comment for why).
func TestManager_SocketNoOpsOnWindows(t *testing.T) {
	dir := t.TempDir()
	r := newFakeRenderer()
	m := NewManager(r)
	m.stateDir = dir
	t.Cleanup(m.Shutdown)

	out := m.Show("12345678", time.Hour, "gr_1", "")
	if !out.Delivered {
		t.Fatalf("expected console delivery via the fake renderer to still succeed, got %+v", out)
	}
	if m.listener != nil {
		t.Fatalf("expected no pull-command listener on Windows, got %+v", m.listener)
	}
	if _, err := os.Stat(filepath.Join(dir, socketFileName)); !os.IsNotExist(err) {
		t.Fatalf("expected no socket file to be created on Windows, stat err=%v", err)
	}

	m.Clear("gr_1") // must not panic despite listener always being nil here
}

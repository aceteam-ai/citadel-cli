// cmd/expose_test.go
//
// Pins the fix for citadel-cli#429 Part 2: `citadel expose --check` must
// probe OWN-NODE services over loopback (127.0.0.1), not this node's own
// AceTeam Network (tsnet) mesh IP. Userspace tsnet cannot dial its own mesh
// IP back to a host-bound docker-proxy socket on the same machine (a known
// tsnet self-dial artifact), so dialing the mesh IP for an own-node check
// reported a perfectly healthy service as unreachable. Peer checks are
// unaffected and must keep dialing the peer's actual mesh IP.
package cmd

import (
	"bytes"
	"net"
	"testing"

	"github.com/fatih/color"
)

func TestResolveServiceCheckTarget(t *testing.T) {
	cases := []struct {
		name      string
		isOwnNode bool
		ip        string
		port      int
		wantAddr  string
		wantMesh  bool
	}{
		{
			name:      "own node dials loopback, not its mesh IP",
			isOwnNode: true,
			ip:        "100.64.0.52",
			port:      8201,
			wantAddr:  "127.0.0.1:8201",
			wantMesh:  false,
		},
		{
			name:      "peer dials the peer's actual mesh IP",
			isOwnNode: false,
			ip:        "100.64.0.99",
			port:      8200,
			wantAddr:  "100.64.0.99:8200",
			wantMesh:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveServiceCheckTarget(tc.isOwnNode, tc.ip, tc.port)
			if got.Addr != tc.wantAddr {
				t.Errorf("Addr = %q, want %q", got.Addr, tc.wantAddr)
			}
			if got.UseMesh != tc.wantMesh {
				t.Errorf("UseMesh = %v, want %v", got.UseMesh, tc.wantMesh)
			}
		})
	}
}

// TestCheckServicePort_OwnNodeReachableViaLoopback is the direct regression
// test for the reported bug: a real, healthy own-node listener must be
// reported reachable. Before the fix, an own-node check dialed the mesh IP
// passed in `ip` via network.Dial regardless of whether anything was
// actually listening there over the mesh -- exactly the false negative
// #429 Part 2 describes. The mesh IP here is deliberately a bogus,
// undialable address to prove the own-node path never routes through it.
func TestCheckServicePort_OwnNodeReachableViaLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start test listener: %v", err)
	}
	defer ln.Close()

	// Accept and immediately close connections so the dial succeeds.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port

	out := captureStdoutForExposeTest(t, func() {
		// A bogus, non-dialable "mesh IP" -- if the own-node path ever
		// regresses to dialing this over the mesh, the check would hang or
		// fail, not read as reachable.
		checkServicePort("100.64.0.253", port, "testsvc", true)
	})

	if !bytes.Contains(out, []byte("reachable")) || bytes.Contains(out, []byte("unreachable")) {
		t.Errorf("expected own-node loopback-reachable service to report reachable, got: %q", out)
	}
}

// TestCheckServicePort_OwnNodeUnreachablePortStillReportsUnreachable pins
// the honest-failure side: an own-node port nothing is listening on must
// still report unreachable (the fix must not make every own-node check
// vacuously "reachable").
func TestCheckServicePort_OwnNodeUnreachablePortStillReportsUnreachable(t *testing.T) {
	// Reserve a port, then close it immediately so nothing listens there.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve a port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	out := captureStdoutForExposeTest(t, func() {
		checkServicePort("100.64.0.253", port, "testsvc", true)
	})

	if !bytes.Contains(out, []byte("unreachable")) {
		t.Errorf("expected closed own-node port to report unreachable, got: %q", out)
	}
}

// captureStdoutForExposeTest captures everything checkServicePort prints.
// checkServicePort reports via the package-level goodColor/badColor
// (github.com/fatih/color), which write through color.Output -- a writer
// captured ONCE at package init, not re-read from the os.Stdout variable on
// every call. Swapping os.Stdout itself therefore would not intercept it;
// color.Output must be swapped directly.
func captureStdoutForExposeTest(t *testing.T, fn func()) []byte {
	t.Helper()

	var buf bytes.Buffer
	orig := color.Output
	color.Output = &buf
	defer func() { color.Output = orig }()

	fn()

	return buf.Bytes()
}

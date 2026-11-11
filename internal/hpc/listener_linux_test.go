//go:build linux

package hpc

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const procTCPHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

func TestListenerUIDsParsesListenSocketsOnly(t *testing.T) {
	tcp := procTCPHeader +
		"   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 11 1 0 100 0 0 10 0\n" +
		"   1: 0100007F:1F91 00000000:0000 0A 00000000:00000000 00:00000000 00000000  2000        0 12 1 0 100 0 0 10 0\n" +
		"   2: 0100007F:1F90 0100007F:9999 01 00000000:00000000 00:00000000 00000000  3000        0 13 1 0 100 0 0 10 0\n"
	tcp6 := procTCPHeader +
		"   0: 00000000000000000000000000000000:1F90 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  4000        0 14 1 0 100 0 0 10 0\n"
	got := ListenerUIDs([]string{tcp, tcp6}, 0x1F90)
	if len(got) != 2 || got[0] != 1000 || got[1] != 4000 {
		t.Fatalf("uids = %v (established sockets and other ports must be ignored)", got)
	}
}

func TestCheckOwnersRefusesAForeignListener(t *testing.T) {
	foreign := procTCPHeader + "   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  2000        0 11 1 0 100 0 0 10 0\n"
	mine := strings.ReplaceAll(foreign, " 2000 ", " 1000 ")
	if listening, err := checkOwners([]string{foreign}, 0x1F90, 1000); err == nil || !listening {
		t.Fatalf("foreign listener accepted: %v %v", listening, err)
	}
	if listening, err := checkOwners([]string{mine}, 0x1F90, 1000); err != nil || !listening {
		t.Fatalf("own listener refused: %v %v", listening, err)
	}
	if listening, err := checkOwners([]string{procTCPHeader}, 0x1F90, 1000); err != nil || listening {
		t.Fatalf("nothing listening: %v %v", listening, err)
	}
	// A mixed set is refused even when one listener is ours.
	if _, err := checkOwners([]string{foreign, mine}, 0x1F90, 1000); err == nil {
		t.Fatal("a foreign listener beside ours was accepted")
	}
}

// TestCheckListenerOwnerRealSocket reads the real /proc: a listener this
// process opened is owned by this uid, and the wrong uid is refused.
func TestCheckListenerOwnerRealSocket(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	if listening, err := CheckListenerOwner(port, os.Geteuid())(); err != nil || !listening {
		t.Fatalf("own listener: %v %v", listening, err)
	}
	if _, err := CheckListenerOwner(port, os.Geteuid()+1)(); err == nil {
		t.Fatal("a listener of another uid was accepted")
	}
}

func TestWaitModelsReadyOwnedRefusesForeignServer(t *testing.T) {
	var probed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probed = true
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	foreign := func() (bool, error) { return true, context.DeadlineExceeded }
	if err := WaitModelsReadyOwned(context.Background(), srv.URL, time.Millisecond, nil, foreign); err == nil {
		t.Fatal("a foreign listener was accepted as ready")
	}
	if probed {
		t.Fatal("the foreign server was probed")
	}
	// Not listening yet: keep waiting, never probe, until ctx ends.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := WaitModelsReadyOwned(ctx, srv.URL, time.Millisecond, nil, func() (bool, error) { return false, nil }); err == nil || probed {
		t.Fatalf("err=%v probed=%v", err, probed)
	}
}

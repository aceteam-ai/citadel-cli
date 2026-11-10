//go:build linux

package cmd

import (
	"bufio"
	"context"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// listeningSockets returns this process's LISTEN-state TCP sockets as
// inode -> local IP, read from /proc (the in-process equivalent of
// `ss -ltn` filtered to our own descriptors).
func listeningSockets(t *testing.T) map[string]net.IP {
	t.Helper()
	owned := map[string]bool{}
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no /proc: %v", err)
	}
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name()))
		if err == nil && strings.HasPrefix(target, "socket:[") {
			owned[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] = true
		}
	}
	out := map[string]net.IP{}
	for _, table := range []string{"/proc/self/net/tcp", "/proc/self/net/tcp6"} {
		f, err := os.Open(table)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Scan() // header
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 10 || fields[3] != "0A" || !owned[fields[9]] {
				continue
			}
			out[fields[9]] = procHexIP(strings.SplitN(fields[1], ":", 2)[0])
		}
		f.Close()
	}
	return out
}

// procHexIP decodes /proc/net/tcp's little-endian-per-word address.
func procHexIP(s string) net.IP {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil
	}
	for i := 0; i+4 <= len(b); i += 4 {
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	return net.IP(b)
}

// TestHPCWorkerOpensNoNonLoopbackListener runs the HPC worker core and checks
// that no listening socket appears on a non-loopback address while it runs.
func TestHPCWorkerOpensNoNonLoopbackListener(t *testing.T) {
	before := listeningSockets(t)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- runHPCWorker(ctx, testHPCDeps(t, newHPCFakeSource())) }()
	time.Sleep(300 * time.Millisecond)
	after := listeningSockets(t)
	cancel()
	<-errCh
	for inode, ip := range after {
		if _, existed := before[inode]; existed {
			continue
		}
		if ip == nil || !ip.IsLoopback() {
			t.Fatalf("the hpc worker opened a non-loopback listener on %v", ip)
		}
	}
}

//go:build linux

package cmd

import (
	"bufio"
	"context"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/hpc"
	"github.com/aceteam-ai/citadel-cli/internal/network"
)

// listeningSockets returns this process's listening sockets as inode -> local
// IP (nil for unix sockets), read from /proc (the in-process equivalent of
// `ss -lxtn` filtered to our own descriptors): TCP LISTEN state plus unix
// sockets with SO_ACCEPTCON set.
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
	if f, err := os.Open("/proc/self/net/unix"); err == nil {
		sc := bufio.NewScanner(f)
		sc.Scan() // header
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			// Num RefCount Protocol Flags Type St Inode [Path]
			if len(fields) < 7 || !owned[fields[6]] {
				continue
			}
			if flags, err := strconv.ParseUint(fields[3], 16, 32); err == nil && flags&0x00010000 != 0 {
				out[fields[6]] = nil
			}
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

// TestHPCWorkProcessOpensNoNewListener runs the production entry point
// (runHPCWorkProcess: lock, credentials, API source, usage syncer, heartbeat,
// runner) against a fake API and asserts no listening socket of any kind
// appears: no TCP on any address (loopback included) and no unix socket.
func TestHPCWorkProcessOpensNoNewListener(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	defer api.Close()

	root := t.TempDir()
	layout := hpc.Layout{
		Root: root, JobID: "5",
		ConfigDir: filepath.Join(root, "config"), HomeDir: filepath.Join(root, "home"),
		NodeDir: filepath.Join(root, "node"), MachineDir: filepath.Join(root, "machine"),
		WorkspaceDir: filepath.Join(root, "workspace"),
	}
	for _, d := range append(layout.Dirs(), filepath.Join(layout.NodeDir, "network")) {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := "device_api_token: tok\napi_base_url: " + api.URL + "\norg_id: org-1\nfabric_node_id: " + testHPCNodeID + "\n"
	if err := os.WriteFile(filepath.Join(layout.NodeDir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	restore, err := network.OverrideStateDirectoriesForTesting(layout.NodeDir, layout.MachineDir)
	if err != nil {
		t.Fatal(err)
	}
	origDirFn := nodeConfigDirFn
	nodeConfigDirFn = func() string { return layout.NodeDir }
	t.Setenv("HOME", layout.HomeDir)
	defer func() {
		nodeConfigDirFn = origDirFn
		if err := restore(); err != nil {
			t.Error(err)
		}
	}()

	before := listeningSockets(t)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- runHPCWorkProcess(ctx, layout) }()
	time.Sleep(1500 * time.Millisecond)
	after := listeningSockets(t)
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runHPCWorkProcess: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runHPCWorkProcess did not stop")
	}
	for inode, ip := range after {
		if _, existed := before[inode]; !existed {
			t.Fatalf("the hpc worker opened a new listener (inode %s, addr %v)", inode, ip)
		}
	}
}

// TestListeningSocketsSeesTCPAndUnix guards the detector the zero-listener
// test depends on: it must report both kinds when they exist.
func TestListeningSocketsSeesTCPAndUnix(t *testing.T) {
	before := listeningSockets(t)
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	sock := filepath.Join(t.TempDir(), "s")
	unix, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close()
	if got := len(listeningSockets(t)) - len(before); got != 2 {
		t.Fatalf("detector saw %d new listeners, want 2", got)
	}
}

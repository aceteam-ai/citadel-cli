package network

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSessionControllerAndTUNAreDistinctOnSharedPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixed global named pipe cannot be isolated in parallel tests")
	}
	dir := t.TempDir()
	ln, err := ListenSessionControl(dir)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /citadel/session/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mode":"presence"}`))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer func() { srv.Close(); CloseSessionControl(ln, dir) }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resp, err := SessionControlRequest(ctx, dir, http.MethodGet, "/citadel/session/v1/status")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("session status = %v, %v", resp, err)
	}
	resp.Body.Close()
	if localAPIReachable(LocalAPISocketPath(dir)) {
		t.Fatal("session controller misclassified as TUN localapi")
	}
	if kind, err := probeLocalControlEndpoint(dir); err != nil || kind != "session" {
		t.Fatalf("controller kind = %q, %v; want session", kind, err)
	}
	mode, err := SelectBackend(dir)
	if err != nil || mode != ModeUserspace {
		t.Fatalf("SelectBackend = %q, %v; want userspace", mode, err)
	}
	if _, err := ListenSessionControl(dir); err == nil {
		t.Fatal("second controller replaced a live endpoint")
	}
	if _, err := os.Stat(LocalAPISocketPath(dir)); err != nil {
		t.Fatalf("live endpoint was unlinked: %v", err)
	}
}

func TestSessionControllerRefusesTUNAndRecoversStaleSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixed global named pipe cannot be isolated in parallel tests")
	}
	dir := t.TempDir()
	path := LocalAPISocketPath(dir)
	ln, err := listenLocalAPI(path)
	if err != nil {
		t.Fatal(err)
	}
	tun := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/localapi/v0/status" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Self":{}}`))
			return
		}
		http.NotFound(w, r)
	})}
	go tun.Serve(ln)
	if !localAPIReachable(path) {
		t.Fatal("TUN localapi not detected")
	}
	if kind, err := probeLocalControlEndpoint(dir); err != nil || kind != "tun" {
		t.Fatalf("TUN kind = %q, %v", kind, err)
	}
	if mode, err := SelectBackend(dir); err != nil || mode != ModeAttached {
		t.Fatalf("TUN SelectBackend = %q, %v; want attached", mode, err)
	}
	if _, err := ListenSessionControl(dir); err == nil {
		t.Fatal("session attempted to replace live TUN")
	}
	tun.Close()
	ln.Close()
	// Simulate a crash with an orphaned Unix socket. safesocket itself
	// normally unlinks on graceful Close, so seed the crash artifact directly.
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	stale.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected stale socket: %v", err)
	}
	replacement, err := ListenSessionControl(dir)
	if err != nil {
		t.Fatalf("stale socket not recovered: %v", err)
	}
	CloseSessionControl(replacement, dir)
	if _, err := os.Stat(filepath.Join(dir, "tun.sock")); !os.IsNotExist(err) {
		t.Fatalf("socket survived close: %v", err)
	}
}

func TestUnknownLiveLocalEndpointFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixed global named pipe cannot be isolated in parallel tests")
	}
	dir := t.TempDir()
	path := LocalAPISocketPath(dir)
	ln, err := listenLocalAPI(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.NotFoundHandler()}
	go srv.Serve(ln)
	defer func() { srv.Close(); ln.Close() }()
	if kind, err := probeLocalControlEndpoint(dir); err != nil || kind != "unknown" {
		t.Fatalf("unknown endpoint = %q, %v", kind, err)
	}
	if _, err := SelectBackend(dir); err == nil {
		t.Fatal("unknown live endpoint allowed a second mesh backend")
	}
	if _, err := ListenSessionControl(dir); err == nil {
		t.Fatal("unknown live endpoint was replaced")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unknown endpoint was unlinked: %v", err)
	}
}

func TestSessionBindCannotUnlinkLiveEndpoint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixed global named pipe cannot be isolated in parallel tests")
	}
	dir := t.TempDir()
	path := LocalAPISocketPath(dir)
	owner, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if replacement, err := listenLocalAPIWithoutCleanup(path); err == nil {
		replacement.Close()
		t.Fatal("session bind replaced a live endpoint")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("session bind unlinked the live endpoint: %v", err)
	}
	client, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatalf("original listener is no longer reachable: %v", err)
	}
	client.Close()
}

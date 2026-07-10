package network

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// echoServer accepts one connection and echoes everything back.
func echoServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return ln
}

func TestRunTCPProxyPipesBothDirections(t *testing.T) {
	echo := echoServer(t)
	defer echo.Close()

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = RunTCPProxy(ctx, proxyLn, echo.Addr().String()) }()

	conn, err := net.Dial("tcp", proxyLn.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	payload := []byte("livekit-signaling-bytes")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(payload))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echoed %q, want %q", got, payload)
	}
}

// A dead target must close the downstream promptly — that closure IS the
// error signal the platform relay classifies as "SFU not running".
func TestRunTCPProxyDeadTargetClosesDownstream(t *testing.T) {
	// Reserve an address with nothing listening.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	deadAddr := dead.Addr().String()
	dead.Close()

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = RunTCPProxy(ctx, proxyLn, deadAddr) }()

	conn, err := net.Dial("tcp", proxyLn.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("expected downstream close/EOF for dead target, got data")
	}
}

func TestRunTCPProxyStopsOnContextCancel(t *testing.T) {
	echo := echoServer(t)
	defer echo.Close()

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- RunTCPProxy(ctx, proxyLn, echo.Addr().String()) }()

	cancel()
	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("RunTCPProxy returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunTCPProxy did not stop on cancel")
	}
}

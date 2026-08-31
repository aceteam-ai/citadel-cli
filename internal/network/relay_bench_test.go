package network

import (
	"context"
	"io"
	"net"
	"testing"
)

// benchEchoServer accepts connections and echoes bytes back, sized for
// throughput benchmarking (larger copy buffer than the tiny test echo).
func benchEchoServer(b *testing.B) net.Listener {
	b.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("echo listen: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 64*1024)
				_, _ = io.CopyBuffer(conn, conn, buf)
			}()
		}
	}()
	return ln
}

// roundTrip writes payload and reads it fully back over conn.
func roundTrip(b *testing.B, conn net.Conn, payload, readBuf []byte) {
	b.Helper()
	if _, err := conn.Write(payload); err != nil {
		b.Fatalf("write: %v", err)
	}
	if _, err := io.ReadFull(conn, readBuf); err != nil {
		b.Fatalf("read: %v", err)
	}
}

// BenchmarkDirectLoopback measures a loopback echo round-trip with NO relay in
// the path. This is the baseline the relay overhead is measured against.
//
// NOTE: this isolates the relay's copy-pump cost only. It does NOT traverse the
// tsnet userspace netstack or WireGuard, which is the dominant cost on a real
// mesh path and cannot be measured on a single un-enrolled host. See the #502
// findings comment for the two-node measurement plan.
func BenchmarkDirectLoopback(b *testing.B) {
	echo := benchEchoServer(b)
	defer echo.Close()

	conn, err := net.Dial("tcp", echo.Addr().String())
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	const size = 64 * 1024
	payload := make([]byte, size)
	readBuf := make([]byte, size)
	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		roundTrip(b, conn, payload, readBuf)
	}
}

// BenchmarkThroughRelay measures the same round-trip THROUGH ServeRelay
// (loopback -> relay pipe -> loopback echo). The delta vs BenchmarkDirectLoopback
// is the relay's per-connection copy overhead.
func BenchmarkThroughRelay(b *testing.B) {
	echo := benchEchoServer(b)
	defer echo.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("relay listen: %v", err)
	}
	target := echo.Addr().String()
	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", target)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ServeRelay(ctx, ln, dial, nil) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		b.Fatalf("dial relay: %v", err)
	}
	defer conn.Close()

	const size = 64 * 1024
	payload := make([]byte, size)
	readBuf := make([]byte, size)
	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		roundTrip(b, conn, payload, readBuf)
	}
}

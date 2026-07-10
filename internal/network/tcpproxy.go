package network

import (
	"context"
	"io"
	"net"
	"time"
)

// dialTimeout bounds the per-connection upstream dial. The target is always
// loopback, so a healthy service answers instantly; the timeout only trims
// how long a dead target holds the downstream open.
const dialTimeout = 5 * time.Second

// RunTCPProxy accepts connections on ln and pipes each to a fresh TCP
// connection to targetAddr until ctx is cancelled. The dial is lazy and
// per-connection, so a target that starts after the proxy (e.g. the livekit
// container coming up on its host port) is picked up automatically — the
// same posture as the websockify bridge's lazy VNC dial.
//
// This is what exposes a host-bound service port over the tsnet mesh: tsnet
// is userspace networking, so a docker host-network port is NOT reachable at
// the node's VPN IP unless the citadel process explicitly listens there and
// pipes the bytes (see services/ports.go LiveKitWSPort).
func RunTCPProxy(ctx context.Context, ln net.Listener, targetAddr string) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		go proxyTCPConn(ctx, conn, targetAddr)
	}
}

func proxyTCPConn(ctx context.Context, downstream net.Conn, targetAddr string) {
	defer downstream.Close()

	dialer := net.Dialer{Timeout: dialTimeout}
	upstream, err := dialer.DialContext(ctx, "tcp", targetAddr)
	if err != nil {
		// Nothing listening on the target (service not installed/running).
		// Closing the downstream is the whole error signal, matching what a
		// direct connection refusal would look like to the dialer.
		return
	}
	defer upstream.Close()

	done := make(chan struct{}, 2)
	pump := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		// Half-close where the transport supports it so the peer sees EOF
		// and can finish its own write side (WebSocket close handshake).
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go pump(upstream, downstream)
	go pump(downstream, upstream)

	// Wait for one direction to finish, then give the other a bounded drain
	// before the deferred closes tear the pipe down.
	<-done
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
	}
}

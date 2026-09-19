package network

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"testing"
	"time"
)

func TestParseRelayRuleIngressShorthand(t *testing.T) {
	rule, err := ParseRelayRule("7880:7880")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rule.Direction != Ingress {
		t.Errorf("direction = %q, want ingress", rule.Direction)
	}
	if rule.ListenPort != 7880 || rule.TargetPort != 7880 {
		t.Errorf("ports = %d->%d, want 7880->7880", rule.ListenPort, rule.TargetPort)
	}
	if rule.TargetHost != "127.0.0.1" {
		t.Errorf("target host = %q, want 127.0.0.1", rule.TargetHost)
	}
	if rule.TLS {
		t.Error("TLS should be false")
	}
	if rule.TargetAddr() != "127.0.0.1:7880" {
		t.Errorf("target addr = %q", rule.TargetAddr())
	}
}

func TestParseRelayRuleIngressTLS(t *testing.T) {
	rule, err := ParseRelayRule("8206:8206:tls")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !rule.TLS {
		t.Error("TLS should be true")
	}
	if rule.ListenPort != 8206 || rule.TargetPort != 8206 {
		t.Errorf("ports = %d->%d, want 8206->8206", rule.ListenPort, rule.TargetPort)
	}
}

func TestParseRelayRuleEgress(t *testing.T) {
	rule, err := ParseRelayRule("egress/9000:100.64.0.5:8206")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rule.Direction != Egress {
		t.Errorf("direction = %q, want egress", rule.Direction)
	}
	if rule.ListenPort != 9000 {
		t.Errorf("listen port = %d, want 9000", rule.ListenPort)
	}
	if rule.TargetHost != "100.64.0.5" || rule.TargetPort != 8206 {
		t.Errorf("target = %s:%d, want 100.64.0.5:8206", rule.TargetHost, rule.TargetPort)
	}
	if rule.TargetAddr() != "100.64.0.5:8206" {
		t.Errorf("target addr = %q", rule.TargetAddr())
	}
}

func TestParseRelayRuleErrors(t *testing.T) {
	cases := map[string]string{
		"empty":                "",
		"non-numeric port":     "abc:7880",
		"port too high":        "70000:7880",
		"port zero":            "0:7880",
		"ingress extra field":  "7880:7880:7880",
		"egress missing host":  "egress/9000:8206",
		"egress tls forbidden": "egress/9000:100.64.0.5:8206:tls",
		"bad direction":        "sideways/9000:8206",
		"egress empty host":    "egress/9000::8206",
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRelayRule(spec); err == nil {
				t.Errorf("ParseRelayRule(%q) succeeded, want error", spec)
			}
		})
	}
}

func TestParseRelayRulesDuplicateListenPort(t *testing.T) {
	_, err := ParseRelayRules([]string{"7880:7880", "7880:9999"})
	if err == nil {
		t.Fatal("expected duplicate ingress listen port to error")
	}
}

func TestParseRelayRulesIngressEgressSamePortOK(t *testing.T) {
	// Same port number is fine across directions: they bind different ifaces.
	rules, err := ParseRelayRules([]string{"8206:8206", "egress/8206:100.64.0.5:8206"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want 2", len(rules))
	}
}

func TestValidateRejectsEgressTLS(t *testing.T) {
	rule := RelayRule{Direction: Egress, ListenPort: 9000, TargetHost: "100.64.0.5", TargetPort: 8206, TLS: true}
	if err := rule.Validate(); err == nil {
		t.Error("egress + TLS should fail validation")
	}
}

// startEchoServer returns a listener that echoes everything written to it.
func startEchoServer(t *testing.T) net.Listener {
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

// startRelay wires ServeRelay in front of targetAddr on a fresh loopback
// listener and returns the relay's address.
func startRelay(t *testing.T, ctx context.Context, targetAddr string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("relay listen: %v", err)
	}
	dial := func(dctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(dctx, "tcp", targetAddr)
	}
	go func() { _ = ServeRelay(ctx, ln, dial, nil) }()
	return ln.Addr().String()
}

func TestServeRelayPipesBothDirections(t *testing.T) {
	echo := startEchoServer(t)
	defer echo.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	relayAddr := startRelay(t, ctx, echo.Addr().String())

	conn, err := net.Dial("tcp", relayAddr)
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	defer conn.Close()

	payload := []byte("mesh-s3-object-bytes")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(payload))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echoed %q, want %q", got, payload)
	}
}

// A larger multi-chunk transfer exercises the copy loop beyond a single frame,
// which is the file-workload case the spike cares about.
func TestServeRelayLargeTransfer(t *testing.T) {
	echo := startEchoServer(t)
	defer echo.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	relayAddr := startRelay(t, ctx, echo.Addr().String())

	conn, err := net.Dial("tcp", relayAddr)
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	defer conn.Close()

	const size = 1 << 20 // 1 MiB
	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}

	go func() {
		_, _ = conn.Write(payload)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()

	got := make([]byte, size)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("1 MiB round-trip through relay corrupted bytes")
	}
}

// A dead upstream must close the downstream promptly — that closure IS the
// error signal, matching a direct connection refusal.
func TestServeRelayDeadUpstreamClosesDownstream(t *testing.T) {
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	deadAddr := dead.Addr().String()
	dead.Close() // nothing listens here now

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	relayAddr := startRelay(t, ctx, deadAddr)

	conn, err := net.Dial("tcp", relayAddr)
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("expected downstream close/EOF for dead upstream, got data")
	}
}

func TestServeRelayStopsOnContextCancel(t *testing.T) {
	echo := startEchoServer(t)
	defer echo.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("relay listen: %v", err)
	}
	dial := func(dctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(dctx, "tcp", echo.Addr().String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- ServeRelay(ctx, ln, dial, nil) }()

	cancel()
	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("ServeRelay returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeRelay did not stop on cancel")
	}
}

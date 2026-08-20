// internal/network/relay.go
//
// SPIKE (aceteam-ai/citadel-cli#502): a generic in-process relay that pipes
// bytes between a listener and a per-connection upstream dial. It generalizes
// the LiveKit-specific TCP proxy from #482 (reverted in #491 as over-scoped for
// huddles) into a direction-aware, configurable relay.
//
// Why this exists: on default nodes citadel runs tsnet in USERSPACE mode, so the
// node's mesh IP (100.64.x.x) has no kernel interface. External daemons and
// `docker -p <mesh-ip>:` cannot bind it, and a container (which has no TUN)
// cannot route to a peer's mesh IP. The relay bridges both gaps in-process:
//
//	Ingress  (expose a local service on the mesh):
//	    tsnet.Listen(meshIP:port) --pipe--> net.Dial(127.0.0.1:localPort)
//	    e.g. Mesh S3 (VersityGW), node-hosted file services.
//
//	Egress   (let a container consume a mesh service via a local endpoint):
//	    net.Listen(dockerBridgeIP:port) --pipe--> tsnet.Dial(peerMeshIP:port)
//	    e.g. a workload container reaching a file service on ANOTHER node,
//	    with no kernel TUN in the container.
//
// The pipe core (pipeConn) is direction-agnostic: it copies between two conns.
// Only the bind address (listener) and the upstream dialer differ between
// ingress and egress, so both are expressed by the same RelayRule + engine.
package network

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// Direction distinguishes the two flows the relay supports. See the package
// doc for the byte paths. The struct/engine express both; the current spike
// wires and benchmarks ingress, and architects egress for production.
type Direction string

const (
	// Ingress exposes a local (loopback) service at the node's mesh IP.
	Ingress Direction = "ingress"
	// Egress exposes a remote mesh service at a local bind address (e.g. the
	// docker bridge) so TUN-less containers can reach it.
	Egress Direction = "egress"
)

// relayDialTimeout bounds each upstream dial. For ingress the target is
// loopback (a healthy service answers instantly); for egress it is a mesh peer
// reached over tsnet, where a few seconds covers a cold WireGuard handshake.
const relayDialTimeout = 5 * time.Second

// relayDrainTimeout bounds how long the second copy direction may keep draining
// after the first has closed, before the deferred teardown fires.
const relayDrainTimeout = 5 * time.Second

// RelayRule is one (bind -> target) mapping. The zero value is invalid; build
// rules via ParseRelayRule or construct explicitly and call Validate.
type RelayRule struct {
	// Direction selects ingress vs egress. Determines the default TargetHost
	// and which listen/dial transport a wired Relay uses.
	Direction Direction

	// ListenPort is the port bound on the listen side. For ingress this is the
	// mesh port (bound at the node's tsnet IP); for egress it is bound on a
	// local interface (docker bridge / loopback).
	ListenPort int

	// TargetHost is the upstream host to dial. For ingress it defaults to
	// 127.0.0.1 (the local service). For egress it is the peer's mesh IP.
	TargetHost string

	// TargetPort is the upstream port to dial.
	TargetPort int

	// TLS requests a TLS-terminating listener (tsnet ListenTLS) on the bind
	// side. Only meaningful for ingress; see the design doc for the SigV4/S3
	// caveat (ListenTLS binds the node's MagicDNS cert, which couples the cert
	// hostname to any host-signing upstream protocol).
	TLS bool
}

// TargetAddr is the host:port the relay dials for this rule.
func (r RelayRule) TargetAddr() string {
	return net.JoinHostPort(r.TargetHost, strconv.Itoa(r.TargetPort))
}

// Validate checks the rule is internally consistent. It does not touch the
// network, so it is safe to call at config-parse time.
func (r RelayRule) Validate() error {
	switch r.Direction {
	case Ingress, Egress:
	default:
		return fmt.Errorf("invalid direction %q (want ingress or egress)", r.Direction)
	}
	if err := validatePort(r.ListenPort); err != nil {
		return fmt.Errorf("listen port: %w", err)
	}
	if err := validatePort(r.TargetPort); err != nil {
		return fmt.Errorf("target port: %w", err)
	}
	if r.TargetHost == "" {
		return fmt.Errorf("target host is empty")
	}
	if r.TLS && r.Direction == Egress {
		return fmt.Errorf("tls is only supported on ingress rules")
	}
	return nil
}

func validatePort(p int) error {
	if p < 1 || p > 65535 {
		return fmt.Errorf("port %d out of range 1-65535", p)
	}
	return nil
}

// ParseRelayRule parses a rule spec. Grammar:
//
//	[direction/]<spec>
//
// where direction is "ingress" (default) or "egress", and <spec> is:
//
//	ingress:  <listenPort>:<targetPort>[:tls]     (target host is 127.0.0.1)
//	egress:   <listenPort>:<targetHost>:<targetPort>
//
// Examples:
//
//	"7880:7880"                     ingress, mesh :7880 -> 127.0.0.1:7880
//	"8206:8206:tls"                 ingress TLS, mesh :8206 -> 127.0.0.1:8206 (Mesh S3)
//	"egress/9000:100.64.0.5:8206"   egress, bridge :9000 -> peer 100.64.0.5:8206
func ParseRelayRule(spec string) (RelayRule, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return RelayRule{}, fmt.Errorf("empty relay rule")
	}

	direction := Ingress
	if slash := strings.IndexByte(spec, '/'); slash >= 0 {
		d := Direction(strings.ToLower(spec[:slash]))
		if d != Ingress && d != Egress {
			return RelayRule{}, fmt.Errorf("invalid direction %q in rule %q", spec[:slash], spec)
		}
		direction = d
		spec = spec[slash+1:]
	}

	fields := strings.Split(spec, ":")

	// A trailing "tls" flag is only valid for ingress.
	tls := false
	if len(fields) > 0 && strings.EqualFold(fields[len(fields)-1], "tls") {
		if direction != Ingress {
			return RelayRule{}, fmt.Errorf("tls flag is only valid on ingress rules: %q", spec)
		}
		tls = true
		fields = fields[:len(fields)-1]
	}

	rule := RelayRule{Direction: direction, TLS: tls}

	switch direction {
	case Ingress:
		if len(fields) != 2 {
			return RelayRule{}, fmt.Errorf("ingress rule wants <listenPort>:<targetPort>[:tls], got %q", spec)
		}
		lp, err := parsePort(fields[0])
		if err != nil {
			return RelayRule{}, fmt.Errorf("listen port in %q: %w", spec, err)
		}
		tp, err := parsePort(fields[1])
		if err != nil {
			return RelayRule{}, fmt.Errorf("target port in %q: %w", spec, err)
		}
		rule.ListenPort = lp
		rule.TargetHost = "127.0.0.1"
		rule.TargetPort = tp
	case Egress:
		if len(fields) != 3 {
			return RelayRule{}, fmt.Errorf("egress rule wants <listenPort>:<targetHost>:<targetPort>, got %q", spec)
		}
		lp, err := parsePort(fields[0])
		if err != nil {
			return RelayRule{}, fmt.Errorf("listen port in %q: %w", spec, err)
		}
		host := strings.TrimSpace(fields[1])
		if host == "" {
			return RelayRule{}, fmt.Errorf("empty target host in %q", spec)
		}
		tp, err := parsePort(fields[2])
		if err != nil {
			return RelayRule{}, fmt.Errorf("target port in %q: %w", spec, err)
		}
		rule.ListenPort = lp
		rule.TargetHost = host
		rule.TargetPort = tp
	}

	if err := rule.Validate(); err != nil {
		return RelayRule{}, err
	}
	return rule, nil
}

func parsePort(s string) (int, error) {
	p, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("not a number: %q", s)
	}
	if err := validatePort(p); err != nil {
		return 0, err
	}
	return p, nil
}

// ParseRelayRules parses multiple specs and rejects duplicate listen ports
// within the same direction (two rules binding the same port would race for
// inbound connections). Ingress and egress may reuse a port number because they
// bind different interfaces.
func ParseRelayRules(specs []string) ([]RelayRule, error) {
	rules := make([]RelayRule, 0, len(specs))
	seen := make(map[string]bool, len(specs))
	for _, spec := range specs {
		rule, err := ParseRelayRule(spec)
		if err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%s/%d", rule.Direction, rule.ListenPort)
		if seen[key] {
			return nil, fmt.Errorf("duplicate %s listen port %d", rule.Direction, rule.ListenPort)
		}
		seen[key] = true
		rules = append(rules, rule)
	}
	return rules, nil
}

// DialFunc opens one upstream connection for a piped session. It is injected so
// ingress (net.Dial loopback) and egress (tsnet Dial to a mesh peer) share the
// same accept/pipe engine, and so tests can supply a plain loopback dialer.
type DialFunc func(ctx context.Context) (net.Conn, error)

// ServeRelay accepts connections on ln and pipes each to a fresh upstream from
// dial until ctx is cancelled or ln fails. The dial is lazy and per-connection,
// so an upstream that starts after the relay (e.g. a service container coming
// up) is picked up automatically. Returns ctx.Err() on cancellation.
func ServeRelay(ctx context.Context, ln net.Listener, dial DialFunc, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
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
		go pipeConn(ctx, conn, dial, logf)
	}
}

// pipeConn dials an upstream and copies bytes bidirectionally between the
// downstream (accepted) and upstream connections until one side closes. A dead
// upstream simply closes the downstream, which is the same signal a direct
// connection refusal produces for the caller.
func pipeConn(ctx context.Context, downstream net.Conn, dial DialFunc, logf func(string, ...any)) {
	defer downstream.Close()

	dialCtx, cancel := context.WithTimeout(ctx, relayDialTimeout)
	upstream, err := dial(dialCtx)
	cancel()
	if err != nil {
		logf("relay: upstream dial failed: %v", err)
		return
	}
	defer upstream.Close()

	done := make(chan struct{}, 2)
	pump := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		// Half-close where the transport supports it so the peer sees EOF and
		// can finish its own write side (e.g. a WebSocket/S3 close handshake).
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
	case <-time.After(relayDrainTimeout):
	case <-ctx.Done():
	}
}

// loopbackDial returns a DialFunc that dials the rule's loopback target with a
// plain net.Dialer. This is the ingress upstream dialer.
func loopbackDial(rule RelayRule) DialFunc {
	target := rule.TargetAddr()
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", target)
	}
}

// meshDial returns a DialFunc that dials the rule's mesh-peer target over the
// tsnet stack via the global server. This is the egress upstream dialer. It is
// the production wiring for egress; the spike does not exercise it end-to-end
// (that needs two enrolled nodes) but the engine accepts it unchanged.
func meshDial(rule RelayRule) DialFunc {
	target := rule.TargetAddr()
	return func(ctx context.Context) (net.Conn, error) {
		return Dial(ctx, "tcp", target)
	}
}

// listenForRule binds the listener for a rule using the production transport:
// ingress binds the node's tsnet mesh IP (ListenVPN / ListenTLS); egress binds
// a local interface (bindHost) with a plain kernel listener. Returns the
// listener and a human-readable bound address for logging.
func listenForRule(rule RelayRule, bindHost string) (net.Listener, string, error) {
	port := strconv.Itoa(rule.ListenPort)
	switch rule.Direction {
	case Ingress:
		if rule.TLS {
			s := Global()
			if s == nil {
				return nil, "", fmt.Errorf("not connected to AceTeam Network")
			}
			// ListenTLS binds the tsnet stack; the empty-host form is matched
			// against the node's mesh IPs at connect time.
			ln, err := s.ListenTLS("tcp", ":"+port)
			if err != nil {
				return nil, "", fmt.Errorf("listen tls on mesh :%s: %w", port, err)
			}
			return ln, "mesh(tls):" + port, nil
		}
		ln, ip, err := ListenVPN("tcp", port)
		if err != nil {
			return nil, "", err
		}
		return ln, net.JoinHostPort(ip, port), nil
	case Egress:
		addr := net.JoinHostPort(bindHost, port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, "", fmt.Errorf("listen on %s: %w", addr, err)
		}
		return ln, addr, nil
	default:
		return nil, "", fmt.Errorf("invalid direction %q", rule.Direction)
	}
}

// dialForRule returns the production upstream dialer for a rule.
func dialForRule(rule RelayRule) DialFunc {
	if rule.Direction == Egress {
		return meshDial(rule)
	}
	return loopbackDial(rule)
}

// Relay runs a set of RelayRules over the wired production transports. Serve
// blocks until ctx is cancelled. EgressBindHost is the local interface egress
// rules bind (e.g. the docker bridge IP so containers can reach it); it
// defaults to 127.0.0.1 when empty.
type Relay struct {
	Rules          []RelayRule
	EgressBindHost string
	Logf           func(string, ...any)
}

// Serve starts every rule's listener and pipe loop, blocking until ctx is
// cancelled. A listener that fails to bind is logged and skipped rather than
// aborting the others, matching the lazy/tolerant posture of the VNC bridge.
func (r *Relay) Serve(ctx context.Context) error {
	logf := r.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	bindHost := r.EgressBindHost
	if bindHost == "" {
		bindHost = "127.0.0.1"
	}
	for _, rule := range r.Rules {
		ln, addr, err := listenForRule(rule, bindHost)
		if err != nil {
			logf("relay: %s rule listen failed: %v", rule.Direction, err)
			continue
		}
		dial := dialForRule(rule)
		logf("relay: %s %s -> %s", rule.Direction, addr, rule.TargetAddr())
		go func(ln net.Listener, dial DialFunc) {
			if err := ServeRelay(ctx, ln, dial, logf); err != nil && err != context.Canceled {
				logf("relay: serve stopped: %v", err)
			}
		}(ln, dial)
	}
	<-ctx.Done()
	return ctx.Err()
}

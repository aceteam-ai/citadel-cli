package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/aceteam-ai/citadel-cli/internal/network"
)

// These seams keep tests off the live mesh and the machine's node config.
var (
	platformListenVPN = network.ListenVPN
	platformWhoIsPeer = network.WhoIsPeer
	platformConfigDir = network.GetNodeConfigDir
	platformMode      = func() network.BackendMode {
		if s := network.Global(); s != nil {
			return s.Mode()
		}
		return "unknown"
	}
)

// platform-exposure.json belongs to the node operator, not EXPOSE_SET. An
// absent file means same-owner only. A malformed file fails closed.
func platformAllowedLogins() (map[string]bool, error) {
	data, err := os.ReadFile(filepath.Join(platformConfigDir(), "platform-exposure.json"))
	if os.IsNotExist(err) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg struct {
		AllowedLogins []string `json:"allowed_logins"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(cfg.AllowedLogins))
	for _, login := range cfg.AllowedLogins {
		login = strings.TrimSpace(login)
		if login == "" {
			return nil, fmt.Errorf("empty allowed login")
		}
		allowed[login] = true
	}
	return allowed, nil
}

// platformTarget admits explicit local/private numeric addresses only. This
// avoids DNS changes and accidental public-network forwarding from a node.
func platformTarget(port int, override string) (string, error) {
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid platform port %d", port)
	}
	if override == "" {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil
	}
	addr, err := netip.ParseAddrPort(override)
	if err != nil || addr.Port() == 0 || !(addr.Addr().IsLoopback() || addr.Addr().IsPrivate()) {
		return "", fmt.Errorf("forward_target must be a loopback or private IP:port")
	}
	return addr.String(), nil
}

type platformExposure struct {
	name      string
	port      int
	ip        string
	ln        net.Listener
	mu        sync.Mutex
	target    string
	closed    bool
	active    map[net.Conn]net.Conn
	serveDone chan struct{}
	wg        sync.WaitGroup
}

// Guarded by exposeOpsMu, including restore and teardown.
var platformExposures = map[string]*platformExposure{}

func platformMeshURL(ip string, port int) string {
	if ip == "" {
		return ""
	}
	return fmt.Sprintf("http://%s/", net.JoinHostPort(ip, strconv.Itoa(port)))
}

func platformURLForPort(port int) string {
	ip := gatewayFactsForURL().MeshIP
	if ip == "" {
		ip = exposeMeshIP()
	}
	return platformMeshURL(ip, port)
}

func startPlatformExposure(name string, port int, target string) (*platformExposure, error) {
	if !gateway.ValidExposeName(name) {
		return nil, fmt.Errorf("invalid exposure name %q", name)
	}
	if _, err := platformTarget(port, target); err != nil {
		return nil, err
	}
	if _, err := platformAllowedLogins(); err != nil {
		return nil, fmt.Errorf("read node platform allowlist: %w", err)
	}
	for other, live := range platformExposures {
		if other != name && live.port == port {
			return nil, fmt.Errorf("mesh port %d is already bound by exposure %q", port, other)
		}
	}
	if live := platformExposures[name]; live != nil && live.port == port {
		live.mu.Lock()
		live.target = target
		live.mu.Unlock()
		return live, nil
	}
	// Bind before replacing the old listener. A collision leaves the existing
	// exposure intact and never silently chooses a different port.
	ln, ip, err := platformListenVPN("tcp", strconv.Itoa(port))
	if err != nil {
		mode := platformMode()
		if mode == network.ModeTUN || mode == network.ModeAttached {
			return nil, fmt.Errorf("platform mesh port %d bind failed in %s mode (a privileged port may require CAP_NET_BIND_SERVICE): %w", port, mode, err)
		}
		return nil, fmt.Errorf("platform mesh port %d bind failed: %w", port, err)
	}
	live := &platformExposure{name: name, port: port, ip: ip, ln: ln, target: target, active: map[net.Conn]net.Conn{}, serveDone: make(chan struct{})}
	old := platformExposures[name]
	platformExposures[name] = live
	if old != nil {
		old.stop()
	}
	go live.serve()
	return live, nil
}

func stopPlatformExposure(name string) bool {
	live := platformExposures[name]
	if live == nil {
		return false
	}
	delete(platformExposures, name)
	live.stop()
	return true
}

func (p *platformExposure) stop() {
	p.mu.Lock()
	p.closed = true
	_ = p.ln.Close()
	for conn, upstream := range p.active {
		_ = conn.Close()
		if upstream != nil {
			_ = upstream.Close()
		}
	}
	p.mu.Unlock()
	<-p.serveDone
	p.wg.Wait()
}

func (p *platformExposure) serve() {
	defer close(p.serveDone)
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		if p.closed || len(p.active) >= 128 {
			p.mu.Unlock()
			_ = conn.Close()
			continue
		}
		p.active[conn] = nil
		p.wg.Add(1)
		p.mu.Unlock()
		go p.forward(conn)
	}
}

func (p *platformExposure) forward(conn net.Conn) {
	defer p.wg.Done()
	defer func() {
		p.mu.Lock()
		delete(p.active, conn)
		p.mu.Unlock()
		_ = conn.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	id, err := platformWhoIsPeer(ctx, conn.RemoteAddr().String())
	cancel()
	if err != nil || id == nil {
		return
	}
	allowed, err := platformAllowedLogins()
	if err != nil {
		Log("platform exposure %q: node allowlist unavailable: %v", p.name, err)
		return
	}
	// SameOwner is derived from control-plane user IDs, but the identity
	// contract still requires a non-empty stable OwnerID before a caller may
	// bind authorization to that relationship. This also fails closed if an
	// incomplete WhoIs response ever compares two zero-value user IDs as equal.
	sameOwner := id.SameOwner && id.OwnerID != ""
	if !sameOwner && !allowed[id.LoginName] {
		Log("platform exposure %q: peer %q denied", p.name, id.NodeName)
		return
	}
	p.mu.Lock()
	target := p.target
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return
	}
	addr, err := platformTarget(p.port, target)
	if err != nil {
		return
	}
	upstream, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.active[conn] = upstream
	p.mu.Unlock()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(upstream, conn)
		if c, ok := upstream.(*net.TCPConn); ok {
			_ = c.CloseWrite()
		}
		close(done)
	}()
	_, _ = io.Copy(conn, upstream)
	if c, ok := conn.(*net.TCPConn); ok {
		_ = c.CloseWrite()
	}
	<-done
}

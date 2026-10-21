package workflow

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPTranslatedIPv6AndMeshDNSCannotBeAllowlisted(t *testing.T) {
	p, err := parseHTTPPolicy([]byte("allow:\n  - host: allowed.example\n  - cidr: fd7a:115c:a1e0::/48\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{
		"64:ff9b::127.0.0.1", "64:ff9b::169.254.169.254", "64:ff9b::8.8.8.8",
		"64:ff9b:1:7f00:0:100::", "64:ff9b:1::808:808",
		"2002:7f00:1::1", "2002:a9fe:a9fe::1", "2002:808:808::1",
		"2001:0:4136:e378:8000:63bf:80ff:fffe", "2001::1",
		"::ffff:0:127.0.0.1", "::ffff:0:169.254.169.254", "::ffff:0:8.8.8.8",
		"fd7a:115c:a1e0::53",
	} {
		t.Run(address, func(t *testing.T) {
			ip := netip.MustParseAddr(address)
			if class, hard := httpAddressClass(ip); class == "" || !hard {
				t.Fatalf("address is not hard-denied: %q/%v", class, hard)
			}
			p.resolve = func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{ip}, nil
			}
			if _, _, err := p.resolveURL(context.Background(), "https://allowed.example"); err == nil {
				t.Fatal("exact host/CIDR exception bypassed hard deny")
			}
			if _, err := parseHTTPPolicy([]byte("allow:\n  - host: " + address + "\n")); err == nil {
				t.Fatal("literal host exception accepted")
			}
			if _, err := parseHTTPPolicy([]byte("allow:\n  - cidr: " + address + "/128\n")); err == nil {
				t.Fatal("literal CIDR exception accepted")
			}
		})
	}
	for _, prefix := range translatedHTTPPrefixes {
		if _, err := parseHTTPPolicy([]byte("allow:\n  - cidr: " + prefix.String() + "\n")); err == nil {
			t.Errorf("translated prefix allowlisted: %s", prefix)
		}
	}
}

func TestHTTPHostnameCharacterPolicy(t *testing.T) {
	for _, host := range []string{"has_underscore.example", "bücher.example", "xn--bücher.example"} {
		if _, err := canonicalHTTPHost(host); err == nil {
			t.Errorf("accepted non-ASCII-DNS host %q", host)
		}
	}
	for _, test := range []struct{ host, want string }{
		{"XN--BCHER-KVA.EXAMPLE.", "xn--bcher-kva.example"},
		{"host-with-dashes.example", "host-with-dashes.example"},
	} {
		if got, err := canonicalHTTPHost(test.host); err != nil || got != test.want {
			t.Errorf("canonical host %q: %q, %v", test.host, got, err)
		}
	}
}

func TestHTTPNodeSanitizesPrivateTransportErrors(t *testing.T) {
	const private = "10.77.88.99"
	p, err := parseHTTPPolicy([]byte("allow:\n  - host: allowed.example\n"))
	if err != nil {
		t.Fatal(err)
	}
	p.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr(private)}, nil
	}
	p.dial = func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP(private), Port: 8123}, Err: errors.New("private-transport-detail")}
	}
	_, err = (&HTTPNodeExecutor{policy: p}).Execute(context.Background(), &Node{ID: "test", Params: map[string]any{"url": "http://allowed.example/?secret=private-query"}}, nil)
	if err == nil {
		t.Fatal("expected transport refusal")
	}
	for _, secret := range []string{private, "8123", "private-transport-detail", "private-query"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("transport detail escaped: %q", err)
		}
	}
	var networkError *net.OpError
	if errors.As(err, &networkError) {
		t.Fatal("raw transport error remains reachable through errors.As")
	}
	if !errors.Is(safeHTTPTransportError(context.Canceled), context.Canceled) || !errors.Is(safeHTTPTransportError(context.DeadlineExceeded), context.DeadlineExceeded) {
		t.Fatal("safe cancellation/timeout identity lost")
	}
	dnsTimeout := &net.DNSError{Name: private, Err: "private-resolver-detail", IsTimeout: true}
	if got := safeHTTPTransportError(dnsTimeout); !errors.Is(got, context.DeadlineExceeded) || strings.Contains(got.Error(), private) {
		t.Fatalf("unsafe timeout: %v", got)
	}
}

func TestHTTPPolicyLoadFailureWarnedOncePerExecutor(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, HTTPPolicyFile), []byte("secret-config-text: invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	p := LoadHTTPPolicy(dir)
	if logs.Len() != 0 || p.loadWhy != "invalid_policy_document" {
		t.Fatalf("load should preserve a reason without logging yet: %q/%s", logs.String(), p.loadWhy)
	}
	e := NewExecutor(ExecutorConfig{HTTP: p})
	for i := 0; i < 3; i++ {
		if _, err := e.nodeRegistry[NodeTypeHTTP].Execute(context.Background(), &Node{ID: "refused", Params: map[string]any{"url": "https://public.example"}}, nil); err == nil {
			t.Fatal("bad policy became permissive")
		}
	}
	text := logs.String()
	if strings.Count(text, "workflow HTTP policy unavailable") != 1 || !strings.Contains(text, "invalid_policy_document") || !strings.Contains(text, `"level":"WARN"`) {
		t.Fatalf("missing/repeated bounded warning: %s", text)
	}
	if strings.Contains(text, dir) || strings.Contains(text, "secret-config-text") {
		t.Fatal("local diagnostic leaked filesystem/YAML details")
	}
	if got := httpPolicyIOReason("open", os.ErrPermission); got != "open_permission_denied" {
		t.Fatalf("lost permission-denied reason: %s", got)
	}
}

func TestHTTPNodeSanitizesResponseReadFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "incomplete-private-body")
	}))
	defer server.Close()
	e := httpExecutorForTestServer(t, server)
	_, err := e.Execute(context.Background(), &Node{ID: "read", Params: map[string]any{"url": "http://public.example/?secret=private-query"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "read response: HTTP transport failure") {
		t.Fatalf("unexpected body-read refusal: %v", err)
	}
	for _, secret := range []string{server.Listener.Addr().String(), "private-query", "incomplete-private-body"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("body-read detail escaped: %v", err)
		}
	}
}

func TestHTTPNodePreservesSafeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := HTTPPolicy{
		resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		},
		dial: func(context.Context, string, string) (net.Conn, error) {
			t.Error("already cancelled request reached dial")
			return nil, context.Canceled
		},
	}
	_, err := (&HTTPNodeExecutor{policy: p}).Execute(ctx, &Node{ID: "cancel", Params: map[string]any{"url": "http://public.example/?secret=private-query"}}, nil)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "private-query") {
		t.Fatalf("unsafe cancellation result: %v", err)
	}
}

func TestHTTPExecuteClampsTimeoutBeforeResolution(t *testing.T) {
	for seconds, want := range map[int]time.Duration{-1: 30 * time.Second, 0: 30 * time.Second, 1: time.Second, 120: 120 * time.Second, 121: 120 * time.Second} {
		t.Run(time.Duration(seconds).String(), func(t *testing.T) {
			started := time.Now()
			checkDeadline := func(ctx context.Context, stage string) {
				t.Helper()
				deadline, ok := ctx.Deadline()
				budget := deadline.Sub(started)
				if !ok || budget < want-time.Second || budget > want+time.Second {
					t.Errorf("%s timeout %s, want about %s", stage, budget, want)
				}
			}
			var lookups, dials atomic.Int32
			p := HTTPPolicy{
				resolve: func(ctx context.Context, _ string) ([]netip.Addr, error) {
					lookups.Add(1)
					checkDeadline(ctx, "resolve")
					return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
				},
				dial: func(_ context.Context, _, _ string) (net.Conn, error) {
					dials.Add(1)
					return nil, errors.New("owned immediate test refusal")
				},
			}
			_, err := (&HTTPNodeExecutor{policy: p}).Execute(context.Background(), &Node{ID: "clamp", Params: map[string]any{"url": "http://public.example", "timeout": seconds}}, nil)
			if err == nil || lookups.Load() != 1 || dials.Load() != 1 {
				t.Fatalf("Execute did not traverse guarded path: lookups=%d dials=%d err=%v", lookups.Load(), dials.Load(), err)
			}
		})
	}
}

func TestHTTPExecuteTimeoutBoundsBlockedDial(t *testing.T) {
	// net/http detaches its dial context from the request deadline so a
	// connection can serve another request. Assert the public Execute budget,
	// not an implementation detail of that detached context.
	started := make(chan struct{})
	release := make(chan struct{})
	dialDone := make(chan struct{})
	p := HTTPPolicy{
		resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		},
		dial: func(context.Context, string, string) (net.Conn, error) {
			defer close(dialDone)
			close(started)
			<-release
			return nil, errors.New("owned released dial refusal")
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	executeDone := make(chan struct{})
	go func() {
		defer close(executeDone)
		_, err := (&HTTPNodeExecutor{policy: p}).Execute(ctx, &Node{ID: "blocked-dial", Params: map[string]any{"url": "http://public.example", "timeout": 1}}, nil)
		result <- err
	}()
	defer func() {
		cancel()
		close(release)
		select {
		case <-executeDone:
		case <-time.After(2 * time.Second):
			t.Error("owned Execute did not join after cancellation")
		}
		select {
		case <-dialDone:
		case <-time.After(2 * time.Second):
			t.Error("owned dial did not join after release")
		}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("Execute never reached owned dial")
	}
	select {
	case err := <-result:
		if ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Execute did not apply its one-second budget: parent=%v, result=%v", ctx.Err(), err)
		}
	case <-ctx.Done():
		t.Fatal("Execute exceeded its own budget while dial remained blocked")
	}
}

func TestHTTPPinnedTLSRoundTripVerifiesCertificateAndSNI(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1)}
	template.DNSNames = []string{"public.example"}
	template.NotBefore = time.Now().Add(-time.Hour)
	template.NotAfter = time.Now().Add(time.Hour)
	template.KeyUsage = x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign
	template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	template.IsCA = true
	template.BasicConstraintsValid = true
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	var requests atomic.Int32
	sni := make(chan string, 8)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Host != "public.example" {
			t.Errorf("lost original HTTP Host: %q", r.Host)
		}
		_, _ = io.WriteString(w, "owned TLS response")
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			sni <- hello.ServerName
			return nil, nil
		},
	}
	server.StartTLS()
	defer server.Close()
	for _, test := range []struct {
		name  string
		roots *x509.CertPool
		ok    bool
	}{
		{"public.example", roots, true},
		{"wrong.example", roots, false},
		{"public.example", x509.NewCertPool(), false},
	} {
		p := HTTPPolicy{
			resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
				if host != test.name {
					t.Errorf("unexpected TLS hostname resolution: %q", host)
				}
				return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
			},
			dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				if address != "8.8.8.8:443" {
					t.Errorf("TLS dial not pinned to validated IP: %q", address)
				}
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			},
		}
		u, ip, err := p.resolveURL(context.Background(), "https://"+test.name)
		if err != nil {
			t.Fatal(err)
		}
		client := p.clientFor(u, ip, 5*time.Second)
		transport := client.Transport.(*http.Transport)
		transport.TLSClientConfig.RootCAs = test.roots // test-owned trust only
		response, err := client.Get(u.String())
		if (err == nil) != test.ok {
			t.Fatalf("TLS %s trusted=%v: %v", test.name, test.ok, err)
		}
		if response != nil {
			data, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || string(data) != "owned TLS response" {
				t.Fatalf("TLS response: %q, %v", data, err)
			}
		}
		client.CloseIdleConnections()
		select {
		case got := <-sni:
			if got != test.name {
				t.Errorf("SNI = %q, want %q", got, test.name)
			}
		default:
			t.Fatal("TLS handshake never sent SNI")
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("invalid name/trust reached HTTP handler: %d requests", requests.Load())
	}
}

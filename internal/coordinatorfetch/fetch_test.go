package coordinatorfetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const fixtureToken = "synthetic-capability.signature"

func fixture(t *testing.T, serve http.HandlerFunc) Hooks {
	t.Helper()
	srv := httptest.NewTLSServer(serve)
	t.Cleanup(srv.Close)
	return Hooks{
		LookupIP: func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		},
		DialIP: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "8.8.8.8:443" {
				t.Errorf("unpinned numeric destination: %s", address)
			}
			return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
		},
		NewTransport:   func() *http.Transport { return srv.Client().Transport.(*http.Transport).Clone() },
		InterfaceAddrs: func() ([]net.Addr, error) { return nil, nil },
	}
}

func reason(t *testing.T, err error, want string) {
	t.Helper()
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Reason() != want {
		t.Fatalf("got %v, want static %s", err, want)
	}
	if strings.Contains(err.Error(), fixtureToken) || strings.Contains(err.Error(), "secret-body") || strings.Contains(err.Error(), "example.com") {
		t.Fatal("refusal leaked a credential, response or URL")
	}
}

func TestFixedRoutesHeadersAndLegacyMaximumSemantics(t *testing.T) {
	for _, endpoint := range []Endpoint{FileFetch, MediaInput} {
		for _, body := range []string{"", "small", strings.Repeat("x", 64)} {
			hooks := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				path := FileFetchPath
				if endpoint == MediaInput {
					path = MediaInputPath
				}
				if r.Method != "GET" || r.URL.Path != path || r.URL.RawQuery != "" ||
					r.Header.Get("Authorization") != "Bearer "+fixtureToken || r.Header.Get("Accept-Encoding") != "identity" {
					t.Error("request changed method, route or credential placement")
				}
				_, _ = io.WriteString(w, body)
			})
			var sink bytes.Buffer
			got, err := Get(context.Background(), "https://example.com/", endpoint, fixtureToken, 64, &sink, hooks)
			if err != nil || sink.String() != body || got.Bytes != int64(len(body)) || got.SHA256 != sha256.Sum256([]byte(body)) {
				t.Fatalf("bounded transfer changed bytes/result: %+v %v", got, err)
			}
		}
	}
}

func TestInvalidConfigurationNeverConnects(t *testing.T) {
	hooks := Hooks{LookupIP: func(context.Context, string) ([]net.IPAddr, error) {
		t.Fatal("invalid request reached DNS")
		return nil, nil
	}}
	for _, origin := range []string{"", "http://example.com", "https://user@example.com", "https://example.com:443", "https://example.com/path", "https://example.com?token=" + fixtureToken, "https://example.com#x", "https://example.com.", "https://example.com\\evil", "https://example.com:"} {
		_, err := Get(context.Background(), origin, MediaInput, fixtureToken, 1, io.Discard, hooks)
		reason(t, err, "untrusted_origin")
	}
	for _, token := range []string{"", ".", "..", "token/extra", "token?query", "token\r\ninjected", strings.Repeat("a", 4097)} {
		_, err := Get(context.Background(), "https://example.com", MediaInput, token, 1, io.Discard, hooks)
		reason(t, err, "invalid_capability")
	}
	if !ValidCapability(strings.Repeat("a", 4096)) {
		t.Fatal("exact token bound refused")
	}
	_, err := Get(context.Background(), "https://example.com", Endpoint(255), fixtureToken, 1, io.Discard, hooks)
	reason(t, err, "invalid_endpoint")
	for _, limit := range []int64{0, -1, MaxBytes + 1, 16<<20 + 1} {
		_, err := Get(context.Background(), "https://example.com", MediaInput, fixtureToken, limit, io.Discard, hooks)
		reason(t, err, "invalid_max_bytes")
	}
}

func TestResponseRefusalsAndSizePlusOne(t *testing.T) {
	for _, tc := range []struct {
		name, encoding, body, want string
		status                     int
		chunked                    bool
	}{
		{"unauthorized", "", "secret-body", "capability_invalid_or_expired", 401, false},
		{"forbidden", "", "secret-body", "capability_invalid_or_expired", 403, false},
		{"gone", "", "secret-body", "capability_invalid_or_expired", 410, false},
		{"redirect", "", "secret-body", "unexpected_status", 302, false},
		{"server", "", "secret-body", "unexpected_status", 500, false},
		{"gzip", "gzip", "data", "content_encoding_refused", 200, false},
		{"known-over", "", "12345", "size_exceeded", 200, false},
		{"chunked-over", "identity", "123456789", "size_exceeded", 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hooks := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != MediaInputPath {
					t.Error("followed redirect or selected another endpoint")
				}
				w.Header().Set("Content-Encoding", tc.encoding)
				w.Header().Set("Location", "https://example.com/elsewhere?token="+fixtureToken)
				w.WriteHeader(tc.status)
				if tc.chunked {
					w.(http.Flusher).Flush()
				}
				_, _ = io.WriteString(w, tc.body)
			})
			var sink bytes.Buffer
			_, err := Get(context.Background(), "https://example.com", MediaInput, fixtureToken, 4, &sink, hooks)
			reason(t, err, tc.want)
			if sink.Len() > 5 || (!tc.chunked && sink.Len() != 0) {
				t.Fatal("refused response exceeded size+1 or wrote before header checks")
			}
		})
	}
}

func TestDNSAllAnswersReconnectOwnAddressAndErrors(t *testing.T) {
	for _, addresses := range [][]string{
		{}, {"127.0.0.1"}, {"10.0.0.1"}, {"169.254.169.254"}, {"100.64.0.1"}, {"::1"},
		{"fd00::1"}, {"64:ff9b::7f00:1"}, {"2002:7f00:1::1"}, {"2001:db8::1"}, {"192.0.2.1"},
		{"198.18.0.1"}, {"240.0.0.1"}, {"8.8.8.8", "127.0.0.1"}, {"invalid"},
	} {
		hooks := Hooks{
			LookupIP: func(context.Context, string) ([]net.IPAddr, error) {
				var ips []net.IPAddr
				for _, address := range addresses {
					ips = append(ips, net.IPAddr{IP: net.ParseIP(address)})
				}
				return ips, nil
			},
			InterfaceAddrs: func() ([]net.Addr, error) { return nil, nil },
			DialIP: func(context.Context, string, string) (net.Conn, error) {
				t.Fatal("dialed a refused DNS answer")
				return nil, nil
			},
		}
		tr := Transport("example.com", hooks)
		if _, err := tr.DialContext(context.Background(), "tcp", "example.com:443"); err == nil {
			t.Fatal("allowed special/mixed DNS answer")
		}
	}
	for _, failure := range []string{"dns", "interface", "own", "connect", "zone"} {
		hooks := Hooks{
			LookupIP: func(context.Context, string) ([]net.IPAddr, error) {
				if failure == "dns" {
					return nil, errors.New(fixtureToken)
				}
				zone := ""
				if failure == "zone" {
					zone = "untrusted"
				}
				return []net.IPAddr{{IP: net.ParseIP("8.8.8.8"), Zone: zone}}, nil
			},
			InterfaceAddrs: func() ([]net.Addr, error) {
				if failure == "interface" {
					return nil, errors.New(fixtureToken)
				}
				if failure == "own" {
					return []net.Addr{&net.IPAddr{IP: net.ParseIP("8.8.8.8")}}, nil
				}
				return nil, nil
			},
			DialIP: func(context.Context, string, string) (net.Conn, error) {
				if failure != "connect" {
					t.Error("dialed after failed destination check")
				}
				return nil, errors.New(fixtureToken)
			},
		}
		_, err := Get(context.Background(), "https://example.com", MediaInput, fixtureToken, 4, io.Discard, hooks)
		want := "destination_blocked"
		if failure == "dns" {
			want = "dns_failed"
		} else if failure == "connect" {
			want = "connect_failed"
		}
		reason(t, err, want)
	}
	lookups, dials := 0, 0
	tr := Transport("example.com", Hooks{
		LookupIP: func(context.Context, string) ([]net.IPAddr, error) {
			lookups++
			ip := "8.8.8.8"
			if lookups > 1 {
				ip = "127.0.0.1"
			}
			return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
		},
		InterfaceAddrs: func() ([]net.Addr, error) { return nil, nil },
		DialIP: func(context.Context, string, string) (net.Conn, error) {
			dials++
			a, b := net.Pipe()
			b.Close()
			return a, nil
		},
	})
	conn, err := tr.DialContext(context.Background(), "tcp", "example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	for _, address := range []string{"example.com:443", "other.example:443", "example.com:80"} {
		if _, err := tr.DialContext(context.Background(), "tcp", address); err == nil {
			t.Fatal("reconnect or changed authority bypassed pinning")
		}
	}
	if lookups != 2 || dials != 1 {
		t.Fatal("DNS was reused or refused answers were dialed")
	}
}

type brokenSink struct{}

func (brokenSink) Write([]byte) (int, error) { return 0, errors.New(fixtureToken) }

func TestSinkTLSAndProxyBoundaries(t *testing.T) {
	hooks := fixture(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("data")) })
	_, err := Get(context.Background(), "https://example.com", MediaInput, fixtureToken, 4, brokenSink{}, hooks)
	reason(t, err, "transfer_failed")
	hooks.NewTransport = nil // The real loopback fixture cert is not publicly trusted.
	_, err = Get(context.Background(), "https://example.com", MediaInput, fixtureToken, 4, io.Discard, hooks)
	reason(t, err, "fetch_failed")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	tr := Transport("example.com", Hooks{})
	if tr.Proxy != nil || !tr.DisableCompression || !tr.DisableKeepAlives || tr.TLSClientConfig != nil {
		t.Fatal("production transport enabled proxy/compression/reuse or custom TLS bypass")
	}
}

func TestCancellationDuringBodyPrecedesPartialResult(t *testing.T) {
	started := make(chan struct{})
	hooks := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("a"))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		_, err := Get(ctx, "https://example.com", MediaInput, fixtureToken, 4, io.Discard, hooks)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("owned response did not begin")
	}
	cancel()
	select {
	case err := <-done:
		reason(t, err, "cancelled_or_timeout")
	case <-time.After(2 * time.Second):
		t.Fatal("body ignored cancellation")
	}
}

package platform

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestRewriteLoopbackWSPort is the load-bearing unit for the container CDP path
// (#514): a containerized Chrome advertises its container-internal debug port in
// the webSocketDebuggerUrl, which is unreachable from the host, so the driver must
// rewrite host:port to the PUBLISHED loopback port before dialing. The /devtools
// path (which identifies the page target) must be preserved verbatim.
func TestRewriteLoopbackWSPort(t *testing.T) {
	cases := []struct {
		name string
		in   string
		port int
		want string
	}{
		{
			name: "container internal port rewritten to published host port",
			in:   "ws://127.0.0.1:9222/devtools/page/AB12CD34",
			port: 8208,
			want: "ws://127.0.0.1:8208/devtools/page/AB12CD34",
		},
		{
			name: "localhost host also normalized to 127.0.0.1",
			in:   "ws://localhost:9222/devtools/page/XY",
			port: 8208,
			want: "ws://127.0.0.1:8208/devtools/page/XY",
		},
		{
			name: "query string preserved",
			in:   "ws://127.0.0.1:9222/devtools/browser?foo=bar",
			port: 8208,
			want: "ws://127.0.0.1:8208/devtools/browser?foo=bar",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rewriteLoopbackWSPort(tc.in, tc.port)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("rewriteLoopbackWSPort(%q, %d) = %q, want %q", tc.in, tc.port, got, tc.want)
			}
		})
	}
}

func TestRewriteLoopbackWSPort_RejectsNonWebSocket(t *testing.T) {
	for _, in := range []string{
		"http://127.0.0.1:9222/devtools/page/AB",
		"://bad",
		"not a url at all %%%",
	} {
		if _, err := rewriteLoopbackWSPort(in, 8208); err == nil {
			t.Errorf("rewriteLoopbackWSPort(%q) expected an error, got nil", in)
		}
	}
}

// TestNewCDPBrowserSatisfiesInterface is a compile-time-ish assertion that
// CDPBrowser exposes the browser surface the MEETING_JOIN flow drives, mirroring
// MeetingBrowser so the same Meet DOM logic runs against either backend.
func TestNewCDPBrowserSatisfiesInterface(t *testing.T) {
	var b interface {
		Navigate(string) error
		CurrentURL() (string, error)
		Evaluate(string) (any, error)
		Type(string, string) error
		Close() error
	} = NewCDPBrowser(8208)
	if b == nil {
		t.Fatal("NewCDPBrowser returned nil")
	}
	if err := b.Close(); err != nil {
		t.Errorf("CDPBrowser.Close() = %v, want nil (meetingd owns the process)", err)
	}
}

func TestCDPBrowserContextMethodsCancelBlockedTargetProbe(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*CDPBrowser, context.Context) error
	}{
		{name: "navigate", call: func(b *CDPBrowser, ctx context.Context) error {
			return b.NavigateContext(ctx, "https://aceteam.ai/huddle-bot/c")
		}},
		{name: "evaluate", call: func(b *CDPBrowser, ctx context.Context) error {
			_, err := b.EvaluateContext(ctx, "1")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				started <- struct{}{}
				<-r.Context().Done()
			}))
			defer srv.Close()
			port := srv.Listener.Addr().(*net.TCPAddr).Port
			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() { result <- tc.call(NewCDPBrowser(port), ctx) }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("CDP target probe did not start")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("context method error = %v, want context.Canceled", err)
				}
			case <-time.After(time.Second):
				t.Fatal("CDP context method did not wake on cancellation")
			}
		})
	}
}

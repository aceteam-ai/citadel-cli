package platform

import "testing"

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

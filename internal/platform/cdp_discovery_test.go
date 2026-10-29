package platform

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type boundedDiscoveryTransport func(*http.Request) (*http.Response, error)

func (f boundedDiscoveryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type boundedDiscoveryBody struct {
	io.Reader
	read, closed int
}

func (b *boundedDiscoveryBody) Read(p []byte) (int, error) {
	n, e := b.Reader.Read(p)
	b.read += n
	return n, e
}
func (b *boundedDiscoveryBody) Close() error { b.closed++; return nil }
func platformDiscoveryFixture(t *testing.T, data string, status int, declared int64) (*boundedDiscoveryBody, *int) {
	t.Helper()
	body := &boundedDiscoveryBody{Reader: strings.NewReader(data)}
	calls := new(int)
	old := cdpHTTPClient
	cdpHTTPClient = &http.Client{Timeout: 5 * time.Second, Transport: boundedDiscoveryTransport(func(r *http.Request) (*http.Response, error) {
		*calls++
		if r.Method != http.MethodGet || r.URL.String() != "http://127.0.0.1:48208/json" {
			return nil, errors.New("unexpected probe origin")
		}
		if _, ok := r.Context().Deadline(); !ok {
			t.Error("discovery lost deadline")
		}
		body.Reader = strings.NewReader(data)
		return &http.Response{StatusCode: status, ContentLength: declared, Header: make(http.Header), Body: body, Request: r}, nil
	})}
	t.Cleanup(func() { cdpHTTPClient = old })
	return body, calls
}

const platformDiscoveryPage = `{"type":"page","url":"https://accounts.google.com/fixture","webSocketDebuggerUrl":"wss://untrusted.example.invalid:9222/devtools/page/fixture?session=synthetic"}`

func TestPlatformCDPDiscoveryNonPagesBeforePageAndPublishedPort(t *testing.T) {
	data := "[" + strings.Repeat(`{"type":"iframe","url":"https://ad.invalid"},{"type":"worker"},`, 128) + platformDiscoveryPage + "]"
	body, calls := platformDiscoveryFixture(t, data, 200, -1)
	target, err := pickTargetContext(context.Background(), 48208)
	if err != nil || target.Type != "page" || target.URL != "https://accounts.google.com/fixture" || target.WebSocketDebuggerURL != "ws://127.0.0.1:48208/devtools/page/fixture?session=synthetic" {
		t.Fatal("shared discovery changed target or escaped loopback", target, err)
	}
	rewritten, err := rewriteLoopbackWSPort(target.WebSocketDebuggerURL, 48208)
	if err != nil || rewritten != target.WebSocketDebuggerURL || *calls != 1 || body.closed != 1 {
		t.Fatal("container published-port rewrite or body lifetime regressed")
	}
	// The real container browser reads CurrentURL through the same helper.
	browser := NewCDPBrowser(48208)
	got, err := browser.CurrentURL()
	if err != nil || got != target.URL {
		t.Fatal("meeting CurrentURL lost page URL", got, err)
	}
}
func TestPlatformCDPDiscoveryByteAndDeclaredSizeRefuse(t *testing.T) {
	for _, declared := range []int64{-1, 512*1024 + 1} {
		t.Run(map[bool]string{true: "declared", false: "streamed"}[declared > 0], func(t *testing.T) {
			body, _ := platformDiscoveryFixture(t, "["+platformDiscoveryPage+"]"+strings.Repeat(" ", 512*1024), 200, declared)
			target, err := pickTargetContext(context.Background(), 48208)
			if err == nil || !strings.Contains(err.Error(), "512 KiB") || target != (cdpTarget{}) || body.closed != 1 {
				t.Fatal("oversize discovery exposed target", target, err)
			}
			expected := 512*1024 + 1
			if declared > 0 {
				expected = 0
			}
			if body.read != expected {
				t.Fatal("response bound was bypassed", body.read, expected)
			}
		})
	}
}
func TestPlatformCDPDiscoveryOnly200BeforeBodyRead(t *testing.T) {
	for _, status := range []int{201, 204, 302, 404, 500} {
		body, _ := platformDiscoveryFixture(t, "["+platformDiscoveryPage+"]", status, -1)
		target, err := pickTargetContext(context.Background(), 48208)
		if err == nil || target != (cdpTarget{}) || body.read != 0 || body.closed != 1 {
			t.Fatal("non200 discovery used body", status, target, err)
		}
	}
}
func TestPlatformCDPDiscoveryRejectsRedirectWithoutFollowing(t *testing.T) {
	body, calls := platformDiscoveryFixture(t, "[]", 302, -1)
	cdpHTTPClient.CheckRedirect = func(*http.Request, []*http.Request) error { t.Error("shared redirect policy reached"); return nil }
	cdpHTTPClient.Transport = boundedDiscoveryTransport(func(r *http.Request) (*http.Response, error) {
		*calls++
		if *calls > 1 {
			return nil, errors.New("redirect was followed")
		}
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"http://elsewhere.invalid/json"}}, Body: body, Request: r}, nil
	})
	if target, err := pickTargetContext(context.Background(), 48208); err == nil || target != (cdpTarget{}) || *calls != 1 || body.closed != 1 {
		t.Fatal("discovery followed redirect", target, err, *calls)
	}
}
func TestPlatformCDPDiscoveryCountsOnlyPagesAndValidatesSuffix(t *testing.T) {
	for _, suffix := range []string{"", strings.Repeat(`,{"type":"page"}`, 64) + "]", `,{broken]`, "] null"} {
		body, _ := platformDiscoveryFixture(t, "["+platformDiscoveryPage+suffix, 200, -1)
		target, err := pickTargetContext(context.Background(), 48208)
		if err == nil || target != (cdpTarget{}) || body.closed != 1 {
			t.Fatal("useful prefix hid invalid suffix", target, err)
		}
	}
}

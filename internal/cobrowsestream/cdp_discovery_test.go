package cobrowsestream

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

type discoveryRoundTrip func(*http.Request) (*http.Response, error)

func (f discoveryRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type discoveryBody struct {
	source io.Reader
	closes int
	nbytes int
	maxOut int
	caller *testing.T
}

func (b *discoveryBody) Read(buffer []byte) (int, error) {
	if b.maxOut > 0 && len(buffer) > b.maxOut-b.nbytes {
		b.caller.Errorf("reader requested bytes beyond the discovery budget: read=%d request=%d", b.nbytes, len(buffer))
		return 0, errors.New("fixture read budget exceeded")
	}
	n, err := b.source.Read(buffer)
	b.nbytes += n
	return n, err
}

func (b *discoveryBody) Close() error {
	b.closes++
	return nil
}

func fakeDiscoveryClient(t *testing.T, body *discoveryBody, code int, length int64) *http.Client {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	client.Transport = discoveryRoundTrip(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.String() != "http://127.0.0.1:9222/json" {
			t.Fatalf("unexpected discovery request: %s %s", request.Method, request.URL)
		}
		if _, bounded := request.Context().Deadline(); !bounded {
			t.Fatal("discovery dropped its request deadline")
		}
		return &http.Response{StatusCode: code, ContentLength: length, Header: make(http.Header), Body: body, Request: request}, nil
	})
	return client
}

const discoveryPage = `{"type":"page","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/page/first"}`

func TestCDPDiscoveryPreservesFirstPageAndAdditiveFields(t *testing.T) {
	data := `[{"type":"browser","webSocketDebuggerUrl":"ignored"},{"type":"page","newField":{"kept":"compatible"}},` + discoveryPage + `,{"type":"page","webSocketDebuggerUrl":"second"}]`
	body := &discoveryBody{source: strings.NewReader(data)}
	client := fakeDiscoveryClient(t, body, http.StatusOK, -1)
	originalRedirect := func(*http.Request, []*http.Request) error {
		t.Fatal("shared redirect callback was used")
		return nil
	}
	client.CheckRedirect = originalRedirect
	got, err := discoverCDPTargetURL(9222, client)
	if err != nil || got != "ws://127.0.0.1:9222/devtools/page/first" || body.closes != 1 {
		t.Fatalf("first-page compatibility or body closure lost: result=%q error=%v closes=%d", got, err, body.closes)
	}
	// The probe must not replace the shared client's redirect policy.
	if client.CheckRedirect == nil || client.Timeout != 5*time.Second {
		t.Fatal("shared client configuration changed")
	}
}

func TestCDPDiscoveryResponseByteBoundaryBeforeParsing(t *testing.T) {
	const budget = 512 * 1024 // Reviewed discovery ceiling, independent of implementation constants.
	for _, extra := range []int{0, 1, 4096} {
		t.Run("extra="+strconv.Itoa(extra), func(t *testing.T) {
			prefix := "[" + discoveryPage + "]"
			data := prefix + strings.Repeat(" ", budget+extra-len(prefix))
			body := &discoveryBody{source: strings.NewReader(data), maxOut: budget + 1, caller: t}
			got, err := discoverCDPTargetURL(9222, fakeDiscoveryClient(t, body, http.StatusOK, -1))
			if extra == 0 {
				if err != nil || got == "" || body.nbytes != budget {
					t.Fatalf("exact byte ceiling rejected: result=%q error=%v bytes=%d", got, err, body.nbytes)
				}
			} else if err == nil || !strings.Contains(err.Error(), "512 KiB") || got != "" || body.nbytes != budget+1 {
				t.Fatalf("oversize JSON escaped bounded reading: result=%q error=%v bytes=%d", got, err, body.nbytes)
			}
			if body.closes != 1 {
				t.Fatal("response body was not closed")
			}
		})
	}
}

func TestCDPDiscoveryDeclaredLengthCannotReplaceStreamingBound(t *testing.T) {
	for _, declared := range []int64{1, 512*1024 + 1} {
		body := &discoveryBody{source: strings.NewReader("[" + discoveryPage + "]" + strings.Repeat(" ", 512*1024)), maxOut: 512*1024 + 1, caller: t}
		got, err := discoverCDPTargetURL(9222, fakeDiscoveryClient(t, body, http.StatusOK, declared))
		if err == nil || !strings.Contains(err.Error(), "512 KiB") || got != "" || body.closes != 1 {
			t.Fatalf("declared/actual size refusal lost: result=%q error=%v closes=%d", got, err, body.closes)
		}
		if declared > 512*1024 && body.nbytes != 0 || declared == 1 && body.nbytes != 512*1024+1 {
			t.Fatalf("declared-length preflight or streaming bound lost: declared=%d read=%d", declared, body.nbytes)
		}
	}
}

func TestCDPDiscoveryTargetCountValidatesEntireArray(t *testing.T) {
	for _, count := range []int{64, 65} {
		for _, pageFirst := range []bool{false, true} {
			entries := make([]string, count)
			for i := range entries {
				entries[i] = `{"type":"page"}`
			}
			index := count - 1
			if pageFirst {
				index = 0
			}
			entries[index] = discoveryPage
			body := &discoveryBody{source: strings.NewReader("[" + strings.Join(entries, ",") + "]")}
			got, err := discoverCDPTargetURL(9222, fakeDiscoveryClient(t, body, http.StatusOK, -1))
			if count == 64 && (err != nil || got == "") || count == 65 && (err == nil || got != "" || !strings.Contains(err.Error(), "64 page targets")) {
				t.Fatalf("target ceiling or complete-array check lost: count=%d first=%t result=%q error=%v", count, pageFirst, got, err)
			}
			if body.closes != 1 {
				t.Fatal("response body was not closed")
			}
		}
	}
}

func TestCDPDiscoveryFailedStatusCannotSelectPageOrReadBody(t *testing.T) {
	for _, code := range []int{204, 301, 302, 307, 404, 500} {
		body := &discoveryBody{source: strings.NewReader("[" + discoveryPage + "]")}
		got, err := discoverCDPTargetURL(9222, fakeDiscoveryClient(t, body, code, -1))
		if got != "" || err == nil || body.nbytes != 0 || body.closes != 1 {
			t.Fatalf("status %d exposed a target or consumed the error body: result=%q error=%v bytes=%d closes=%d", code, got, err, body.nbytes, body.closes)
		}
	}
}

func TestCDPDiscoveryNonPageEntriesDoNotConsumePageBudget(t *testing.T) {
	for _, entry := range []string{`null`, `{"type":"iframe"}`, `{"type":"worker"}`, `{"type":"browser"}`} {
		body := &discoveryBody{source: strings.NewReader("[" + discoveryPage + strings.Repeat(","+entry, 128) + "]")}
		got, err := discoverCDPTargetURL(9222, fakeDiscoveryClient(t, body, http.StatusOK, -1))
		if err != nil || got != "ws://127.0.0.1:9222/devtools/page/first" || body.closes != 1 {
			t.Fatalf("non-page entries consumed the page budget: entry=%s result=%q error=%v closes=%d", entry, got, err, body.closes)
		}
	}
}

func TestCDPDiscoveryPinsAdvertisedDebuggerAuthority(t *testing.T) {
	for _, advertised := range []string{
		"ws://example.invalid:443/devtools/page/first",
		"wss://example.invalid/devtools/page/first",
		"ws://[::1]:1234/devtools/page/first",
		"ws://127.0.0.1:1/devtools/page/first",
	} {
		body := &discoveryBody{source: strings.NewReader(`[{"type":"page","webSocketDebuggerUrl":"` + advertised + `"}]`)}
		got, err := discoverCDPTargetURL(9222, fakeDiscoveryClient(t, body, http.StatusOK, -1))
		if err != nil || got != "ws://127.0.0.1:9222/devtools/page/first" || body.closes != 1 {
			t.Fatalf("debugger authority escaped the selected local endpoint: result=%q error=%v", got, err)
		}
	}
}

func TestCDPDiscoveryRejectsInvalidDebuggerURL(t *testing.T) {
	for _, advertised := range []string{"relative", "http://example.invalid/page", "ws:///page", "ws://user:password@example.invalid/page", "ws://example.invalid/page#fragment", "ws://[invalid/page"} {
		body := &discoveryBody{source: strings.NewReader(`[{"type":"page","webSocketDebuggerUrl":"` + advertised + `"}]`)}
		got, err := discoverCDPTargetURL(9222, fakeDiscoveryClient(t, body, http.StatusOK, -1))
		if err == nil || got != "" || strings.Contains(err.Error(), advertised) || body.closes != 1 {
			t.Fatalf("invalid debugger URL was not safely refused: result=%q error=%v", got, err)
		}
	}
}

func TestCDPDiscoveryByteLimitPrecedesJSONDecoding(t *testing.T) {
	body := &discoveryBody{source: strings.NewReader("!" + strings.Repeat(" ", 512*1024))}
	got, err := discoverCDPTargetURL(9222, fakeDiscoveryClient(t, body, http.StatusOK, -1))
	if err == nil || got != "" || !strings.Contains(err.Error(), "512 KiB") || body.closes != 1 {
		t.Fatalf("parser ran before its byte bound: result=%q error=%v closes=%d", got, err, body.closes)
	}
}

func TestCDPDiscoveryDoesNotFollowRedirectOrMutateClient(t *testing.T) {
	calls, sharedRedirects := 0, 0
	body := &discoveryBody{source: strings.NewReader("[" + discoveryPage + "]")}
	client := &http.Client{Timeout: 5 * time.Second}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		sharedRedirects++
		return nil
	}
	client.Transport = discoveryRoundTrip(func(request *http.Request) (*http.Response, error) {
		calls++
		if calls != 1 || request.URL.Host != "127.0.0.1:9222" {
			t.Error("discovery followed the endpoint's redirect")
			return nil, errors.New("fixture refused second request")
		}
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"http://example.invalid/elsewhere"}}, Body: body, Request: request}, nil
	})
	if got, err := discoverCDPTargetURL(9222, client); err == nil || got != "" || calls != 1 || sharedRedirects != 0 || body.closes != 1 {
		t.Fatalf("redirect crossed the probe boundary: result=%q error=%v calls=%d callbacks=%d closes=%d", got, err, calls, sharedRedirects, body.closes)
	}
	_ = client.CheckRedirect(nil, nil)
	if sharedRedirects != 1 {
		t.Fatal("probe replaced shared redirect policy")
	}
}

type discoveryFaultReader struct{}

func (discoveryFaultReader) Read([]byte) (int, error) {
	return 0, errors.New("private fixture body read failure")
}

func TestCDPDiscoveryRejectsMalformedSuffixAndReadFailures(t *testing.T) {
	for _, data := range []string{"null", `{}`, `[]`, `[`, "[" + discoveryPage, "[" + discoveryPage + ",", "[" + discoveryPage + ",123]", "[" + discoveryPage + "] []", "[" + discoveryPage + "] trailing"} {
		body := &discoveryBody{source: strings.NewReader(data)}
		got, err := discoverCDPTargetURL(9222, fakeDiscoveryClient(t, body, http.StatusOK, -1))
		if err == nil || got != "" || body.closes != 1 {
			t.Fatalf("invalid discovery accepted: result=%q error=%v closes=%d", got, err, body.closes)
		}
	}
	body := &discoveryBody{source: discoveryFaultReader{}}
	if got, err := discoverCDPTargetURL(9222, fakeDiscoveryClient(t, body, http.StatusOK, -1)); err == nil || got != "" || body.closes != 1 || strings.Contains(err.Error(), "private fixture") {
		t.Fatalf("read failure was not safely refused and closed: result=%q error=%v closes=%d", got, err, body.closes)
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: discoveryRoundTrip(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("private fixture transport failure")
	})}
	if got, err := discoverCDPTargetURL(9222, client); err == nil || got != "" || strings.Contains(err.Error(), "private fixture") {
		t.Fatalf("transport failure was not safely refused: result=%q error=%v", got, err)
	}
}

func TestCDPDiscoveryAdIframeReplyAboveOldByteCeiling(t *testing.T) {
	entries := []string{discoveryPage}
	for i := 0; i < 25; i++ {
		entries = append(entries, `{
  "type": "iframe",
  "url": "https://example.invalid/ad?`+strings.Repeat("x", 6*1024)+`"
}`)
	}
	data := "[\n" + strings.Join(entries, ",\n") + "\n]"
	if len(data) <= 150*1024 || len(data) >= 512*1024 {
		t.Fatalf("fixture does not exercise realistic discovery headroom: %d", len(data))
	}
	body := &discoveryBody{source: strings.NewReader(data)}
	got, err := discoverCDPTargetURL(9222, fakeDiscoveryClient(t, body, http.StatusOK, -1))
	if err != nil || got != "ws://127.0.0.1:9222/devtools/page/first" || body.closes != 1 {
		t.Fatalf("ad iframe discovery reply rejected: result=%q error=%v closes=%d", got, err, body.closes)
	}
}

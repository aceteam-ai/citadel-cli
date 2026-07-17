// internal/jobs/web_fetch_test.go
package jobs

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// loopbackAllowed is the test SSRF policy: it permits loopback (so an httptest
// server on 127.0.0.1 is reachable) but defers to the real policy for every
// other address, so link-local / metadata / private / CGNAT rejection is still
// exercised end to end (including on redirect hops).
func loopbackAllowed(ip net.IP, allowPrivate bool) error {
	if ip.IsLoopback() {
		return nil
	}
	return wfAssertIPAllowed(ip, allowPrivate)
}

func newTestHandler() *WebFetchHandler {
	return &WebFetchHandler{allowIP: loopbackAllowed}
}

func runWebFetch(t *testing.T, h *WebFetchHandler, payload map[string]string) (map[string]any, error) {
	t.Helper()
	out, err := h.Execute(JobContext{}, &nexus.Job{ID: "test", Type: "WEB_FETCH", Payload: payload})
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if jerr := json.Unmarshal(out, &result); jerr != nil {
		t.Fatalf("result is not valid JSON: %v", jerr)
	}
	return result, nil
}

func TestWebFetch_ContractAndHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("ETag", `"abc123"`)
		w.Header().Set("Set-Cookie", "secret=should-not-leak") // not in the relay allowlist
		w.WriteHeader(200)
		_, _ = w.Write([]byte("<html><body>hi</body></html>"))
	}))
	defer srv.Close()

	result, err := runWebFetch(t, newTestHandler(), map[string]string{"url": srv.URL})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := result["status_code"]; got != float64(200) {
		t.Errorf("status_code = %v, want 200", got)
	}
	if got := result["body"]; got != "<html><body>hi</body></html>" {
		t.Errorf("body = %q", got)
	}
	if result["truncated"] != false {
		t.Errorf("truncated = %v, want false", result["truncated"])
	}
	if !strings.HasPrefix(result["final_url"].(string), srv.URL) {
		t.Errorf("final_url = %v, want prefix %v", result["final_url"], srv.URL)
	}
	headers, ok := result["headers"].(map[string]any)
	if !ok {
		t.Fatalf("headers is not an object: %T", result["headers"])
	}
	if ct, _ := headers["content-type"].(string); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type header = %q", headers["content-type"])
	}
	if headers["etag"] != `"abc123"` {
		t.Errorf("etag header = %v", headers["etag"])
	}
	if _, leaked := headers["set-cookie"]; leaked {
		t.Errorf("set-cookie leaked into relayed headers: %v", headers["set-cookie"])
	}
}

func TestWebFetch_TruncationFlag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("A", 5000)))
	}))
	defer srv.Close()

	result, err := runWebFetch(t, newTestHandler(), map[string]string{
		"url":       srv.URL,
		"max_bytes": "2000",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["truncated"] != true {
		t.Errorf("truncated = %v, want true", result["truncated"])
	}
	if body, _ := result["body"].(string); len(body) != 2000 {
		t.Errorf("body length = %d, want 2000", len(body))
	}
}

func TestWebFetch_NotTruncatedAtExactCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("B", 2000)))
	}))
	defer srv.Close()

	result, err := runWebFetch(t, newTestHandler(), map[string]string{
		"url":       srv.URL,
		"max_bytes": "2000",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["truncated"] != false {
		t.Errorf("truncated = %v, want false (body exactly at cap)", result["truncated"])
	}
	if body, _ := result["body"].(string); len(body) != 2000 {
		t.Errorf("body length = %d, want 2000", len(body))
	}
}

func TestWebFetch_ForwardsMethodBodyHeaders(t *testing.T) {
	var gotMethod, gotBody, gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		buf, _ := io.ReadAll(r.Body)
		gotBody = string(buf)
		gotHeader = r.Header.Get("X-Custom")
		w.WriteHeader(201)
	}))
	defer srv.Close()

	result, err := runWebFetch(t, newTestHandler(), map[string]string{
		"url":     srv.URL,
		"method":  "POST",
		"body":    `{"k":"v"}`,
		"headers": `{"X-Custom":"yes"}`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != "POST" {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotBody != `{"k":"v"}` {
		t.Errorf("body = %q", gotBody)
	}
	if gotHeader != "yes" {
		t.Errorf("X-Custom = %q, want yes", gotHeader)
	}
	if result["status_code"] != float64(201) {
		t.Errorf("status_code = %v, want 201", result["status_code"])
	}
}

func TestWebFetch_RedirectFollowedAndFinalURL(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("landed"))
	}))
	defer final.Close()

	start := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer start.Close()

	result, err := runWebFetch(t, newTestHandler(), map[string]string{"url": start.URL})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["body"] != "landed" {
		t.Errorf("body = %v, want landed", result["body"])
	}
	if !strings.HasPrefix(result["final_url"].(string), final.URL) {
		t.Errorf("final_url = %v, want prefix %v", result["final_url"], final.URL)
	}
}

func TestWebFetch_RedirectToBlockedIsRefused(t *testing.T) {
	// Server (loopback, allowed) redirects to the cloud-metadata IP. The dialer
	// re-validates the redirect hop and must refuse it before connecting.
	start := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer start.Close()

	_, err := newTestHandler().Execute(JobContext{}, &nexus.Job{
		ID: "test", Type: "WEB_FETCH", Payload: map[string]string{"url": start.URL},
	})
	if err == nil {
		t.Fatal("expected redirect to metadata IP to be refused, got nil error")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("error = %v, want 'not allowed'", err)
	}
}

func TestWebFetch_NoRedirectWhenDisabled(t *testing.T) {
	start := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.com/next", http.StatusFound)
	}))
	defer start.Close()

	result, err := runWebFetch(t, newTestHandler(), map[string]string{
		"url":              start.URL,
		"follow_redirects": "false",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["status_code"] != float64(302) {
		t.Errorf("status_code = %v, want 302 (redirect not followed)", result["status_code"])
	}
	headers := result["headers"].(map[string]any)
	if headers["location"] != "http://example.com/next" {
		t.Errorf("location header = %v", headers["location"])
	}
}

// --- SSRF policy (pure helper) -------------------------------------------

func TestWebFetch_SSRFPolicy(t *testing.T) {
	cases := []struct {
		name         string
		ip           string
		allowPrivate bool
		wantBlocked  bool
	}{
		{"loopback v4", "127.0.0.1", false, true},
		{"loopback v4 range", "127.5.5.5", false, true},
		{"loopback v6", "::1", false, true},
		{"link-local", "169.254.1.1", false, true},
		{"cloud metadata", "169.254.169.254", false, true},
		{"link-local v6", "fe80::1", false, true},
		{"unspecified v4", "0.0.0.0", false, true},
		{"unspecified v6", "::", false, true},
		{"multicast", "224.0.0.1", false, true},
		{"private 10/8", "10.0.0.5", false, true},
		{"private 172.16/12", "172.16.0.1", false, true},
		{"private 192.168/16", "192.168.1.1", false, true},
		{"ula v6", "fc00::1", false, true},
		{"cgnat mesh", "100.64.0.1", false, true},
		{"private allowed with flag", "10.0.0.5", true, false},
		{"cgnat allowed with flag", "100.64.0.1", true, false},
		{"loopback still blocked with flag", "127.0.0.1", true, true},
		{"metadata still blocked with flag", "169.254.169.254", true, true},
		{"public v4", "93.184.216.34", false, false},
		{"public v6", "2606:2800:220:1:248:1893:25c8:1946", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("bad test IP %q", tc.ip)
			}
			err := wfAssertIPAllowed(ip, tc.allowPrivate)
			if tc.wantBlocked && err == nil {
				t.Errorf("%s: expected blocked, got allowed", tc.ip)
			}
			if !tc.wantBlocked && err != nil {
				t.Errorf("%s: expected allowed, got %v", tc.ip, err)
			}
		})
	}
}

func TestWebFetch_RejectsBadSchemeAndLiteralInternal(t *testing.T) {
	h := &WebFetchHandler{} // real policy
	cases := []string{
		"ftp://example.com/",
		"file:///etc/passwd",
		"http://127.0.0.1/",
		"http://169.254.169.254/",
		"http://10.0.0.1/",
		"http://[::1]/",
		"http://100.64.0.1:8080/",
	}
	for _, u := range cases {
		t.Run(u, func(t *testing.T) {
			_, err := h.Execute(JobContext{}, &nexus.Job{
				ID: "t", Type: "WEB_FETCH", Payload: map[string]string{"url": u},
			})
			if err == nil {
				t.Errorf("expected %q to be refused", u)
			}
		})
	}
}

func TestWebFetch_MissingURL(t *testing.T) {
	_, err := (&WebFetchHandler{}).Execute(JobContext{}, &nexus.Job{
		ID: "t", Type: "WEB_FETCH", Payload: map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error for missing url")
	}
}

func TestWebFetch_MaxBytesCappedToCeiling(t *testing.T) {
	if got := wfClampInt(wfInt("999999999", webFetchDefaultMaxBytes), 1024, webFetchHardMaxBytes); got != webFetchHardMaxBytes {
		t.Errorf("max_bytes clamp = %d, want %d", got, webFetchHardMaxBytes)
	}
	if got := wfClampInt(wfInt("", webFetchDefaultMaxBytes), 1024, webFetchHardMaxBytes); got != webFetchDefaultMaxBytes {
		t.Errorf("default max_bytes = %d, want %d", got, webFetchDefaultMaxBytes)
	}
}

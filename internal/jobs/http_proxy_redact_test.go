package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// Credentials in headers and URLs must never reach the job log in full; a
// last-four fingerprint is logged instead so an operator can tell them apart.
func TestHTTPProxyLogsFingerprintsNotCredentials(t *testing.T) {
	srv, _ := countingServer(t, "ok")
	h := &HTTPProxyHandler{localPorts: map[int]bool{portOf(t, srv.URL): true}}

	const secret1 = "hdr-secret-0123456789abcdefghijWXYZ"
	const secret2 = "qry-secret-0123456789abcdefghijLMNO"
	headers, err := json.Marshal(map[string]string{
		"X-API-Key":    secret1,
		"Content-Type": "application/json",
	})
	if err != nil {
		t.Fatal(err)
	}

	var logs []string
	ctx := JobContext{LogFn: func(level, msg string) { logs = append(logs, msg) }}
	_, err = h.Execute(ctx, &nexus.Job{ID: "proxy", Type: "HTTP_PROXY", Payload: map[string]string{
		"url":     srv.URL + "/sql?token=" + secret2,
		"headers": string(headers),
		"body":    "body-canary",
	}})
	if err != nil {
		t.Fatal(err)
	}

	all := strings.Join(logs, "\n")
	for _, s := range []string{secret1, secret2, "body-canary"} {
		if strings.Contains(all, s) {
			t.Fatalf("log leaked %q: %s", s, all)
		}
	}
	for _, want := range []string{"…WXYZ", "…LMNO", "Content-Type=application/json"} {
		if !strings.Contains(all, want) {
			t.Fatalf("log missing %q: %s", want, all)
		}
	}
	if strings.Index(all, "Content-Type=") > strings.Index(all, "X-API-Key=") {
		t.Fatalf("headers not sorted: %s", all)
	}
}

// Unparseable header JSON still logs the redacted URL and no headers.
func TestHTTPProxyBadHeadersStillLogsRedactedURL(t *testing.T) {
	const secret = "qry-secret-0123456789abcdefghijLMNO"
	var logs []string
	ctx := JobContext{LogFn: func(level, msg string) { logs = append(logs, msg) }}
	h := &HTTPProxyHandler{}
	_, err := h.Execute(ctx, &nexus.Job{ID: "proxy", Payload: map[string]string{
		"url":     "http://127.0.0.1:1/x?api_key=" + secret,
		"headers": "{not json " + secret,
	}})
	if err == nil {
		t.Fatal("expected header parse error")
	}
	all := strings.Join(logs, "\n")
	if strings.Contains(all, secret) || !strings.Contains(all, "…LMNO") || !strings.Contains(all, "headers=[]") {
		t.Fatalf("unexpected log: %s", all)
	}
}

// Real Execute failures must be safe in both the returned job error and logs.
// The closed listener is local to the isolated test network, not a live service.
func TestHTTPProxyErrorPathsRedactCredentials(t *testing.T) {
	const querySecret = "synthetic-query-credential-0123456789TAIL"
	const password = "synthetic-password-0123456789PASS"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	target := "http://alice:" + password + "@" + address + "/x?token=" + querySecret
	for _, mode := range []string{"parse", "refused-connection", "blocked", "cancelled", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			h := &HTTPProxyHandler{localPorts: map[int]bool{port: true}}
			requestURL := target
			var want error
			switch mode {
			case "parse":
				requestURL = "http://alice:" + password + "@example.test/%zz?token=" + querySecret
			case "refused-connection":
				want = syscall.ECONNREFUSED
			case "blocked":
				h.localPorts = map[int]bool{}
				want = errHTTPProxyDestinationBlocked
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			case "deadline":
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
				want = context.DeadlineExceeded
			}
			var logs []string
			_, err := h.Execute(JobContext{Ctx: ctx, LogFn: func(_, text string) { logs = append(logs, text) }}, &nexus.Job{ID: "synthetic", Payload: map[string]string{"url": requestURL}})
			if err == nil {
				t.Fatal("expected request refusal or failure")
			}
			if want != nil && !errors.Is(err, want) {
				t.Fatalf("lost cause identity: got %v want %v", err, want)
			}
			if mode != "parse" && !strings.Contains(err.Error(), "…TAIL") {
				t.Fatal("safe error lost the query credential fingerprint")
			}
			for _, text := range append(logs, err.Error(), fmt.Sprintf("%+v", err)) {
				if strings.Contains(text, querySecret) || strings.Contains(text, password) {
					t.Fatal("credential reached a request log or job error")
				}
			}
		})
	}
}

func TestHTTPProxyURLErrorKeepsCauseWithoutURLSecrets(t *testing.T) {
	const secret = "synthetic-nested-credential-0123456789TAIL"
	target := "http://example.test/x?token=" + secret
	for _, cause := range []error{errHTTPProxyDestinationBlocked, context.Canceled, context.DeadlineExceeded} {
		nested := &url.Error{Op: "Get", URL: target, Err: cause}
		// A transport may wrap a URL error; none of its surrounding raw URL text
		// should survive conversion to the safe returned error.
		outer := fmt.Errorf("unsafe wrapper %s: %w", target, nested)
		safe := redactHTTPProxyURLError(&url.Error{Op: "Get", URL: target, Err: outer})
		if !errors.Is(safe, cause) || strings.Contains(safe.Error(), secret) {
			t.Fatal("nested URL was not redacted while preserving its cause")
		}
	}
}

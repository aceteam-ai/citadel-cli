package jobs

import (
	"encoding/json"
	"strings"
	"testing"

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

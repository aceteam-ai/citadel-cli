//go:build !windows

package hpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestApprovalHTTPErrorDetails(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"zero node quota", 402, `{"code":"fabric_node_limit","error":"Node cap exceeded","limit":0,"current":0}`, "fabric node limit reached (0 of 0)"},
		{"over quota", 402, `{"code":"fabric_node_limit","limit":2,"current":3}`, "fabric node limit reached (3 of 2)"},
		{"quota without counts", 402, `{"code":"fabric_node_limit"}`, "fabric node limit reached"},
		{"quota without current", 402, `{"code":"fabric_node_limit","limit":0}`, "fabric node limit reached"},
		{"negative count", 402, `{"code":"fabric_node_limit","limit":-1,"current":0}`, "fabric node limit reached"},
		{"manager changed", 403, `{"code":"approver_not_manager","error":"Old approver"}`, "approver is no longer an organization manager"},
		{"generic error", 409, `{"code":"unknown_code","error":"Approval already consumed"}`, "Approval already consumed"},
		{"validation detail", 400, `{"error":"Check the job labels"}`, "Check the job labels"},
		{"not found detail", 404, `{"error":"Enrollment request not found"}`, "Enrollment request not found"},
		{"expiry detail", 410, `{"error":"Approval expired"}`, "Approval expired"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			_, createErr := (Client{}).createEnrollRequest(context.Background(), srv.URL, testOrgID, "fake-csr", testLabels())
			_, _, pollErr := (Client{}).pollEnrollRequest(context.Background(), srv.URL, "req_1", testPollSecret)
			for _, err := range []error{createErr, pollErr} {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("missing structured diagnostic %q: %v", tt.want, err)
				}
				var transient *transientPollError
				if errors.As(err, &transient) {
					t.Fatalf("terminal refusal marked retryable: %v", err)
				}
				if tt.name == "negative count" && strings.Contains(err.Error(), "of -1") {
					t.Fatal("invalid count displayed")
				}
			}
		})
	}
}

func TestApprovalHTTPErrorBodyIsBoundedSanitizedAndSecretFree(t *testing.T) {
	for _, create := range []bool{false, true} {
		for _, message := range []string{
			"Denied " + testPollSecret + "\n\ttry again\x1b[2J\u202e",
			"Denied " + testPollSecret[:7] + "\x00" + testPollSecret[7:],
			strings.Repeat("x", 205) + testPollSecret,
			strings.Repeat("界", maxApprovalErrorBytes),
		} {
			body, err := json.Marshal(map[string]any{"error": message, "poll_secret": testPollSecret, "token": "ignored-token-field"})
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(422)
				_, _ = w.Write(body)
			}))
			var responseErr error
			if create {
				_, responseErr = (Client{}).createEnrollRequest(context.Background(), srv.URL, testOrgID, "fake-csr", testLabels())
			} else {
				_, _, responseErr = (Client{}).pollEnrollRequest(context.Background(), srv.URL, "req_1", testPollSecret)
			}
			srv.Close()
			if responseErr == nil {
				t.Fatal("error response accepted")
			}
			got := responseErr.Error()
			if len(got) > 240 || !utf8.ValidString(got) {
				t.Fatalf("diagnostic is not bounded UTF-8: %d bytes", len(got))
			}
			for _, r := range got {
				if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
					t.Fatalf("terminal control left in diagnostic: %q", got)
				}
			}
			for _, forbidden := range []string{testPollSecret, testPollSecret[:12], "ignored-token-field", "poll_secret"} {
				if strings.Contains(got, forbidden) {
					t.Fatalf("sensitive body data in diagnostic: %q", got)
				}
			}
		}
	}
}

func TestApprovalPollSecretIsScrubbedWithoutAnEchoField(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"error": "Denied: " + testPollSecret})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	_, _, err := (Client{}).pollEnrollRequest(context.Background(), srv.URL, "req_1", testPollSecret)
	if err == nil || !strings.Contains(err.Error(), "denied") || strings.Contains(err.Error(), testPollSecret) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("poll secret echo not scrubbed: %v", err)
	}
}

func TestApprovalMalformedErrorBodiesUseSafeFallback(t *testing.T) {
	for _, body := range []string{
		"",
		"<html>" + testPollSecret + "</html>",
		`{"error":`,
		`{"error":{"poll_secret":"` + testPollSecret + `"}}`,
		`{"error":"\n\t\u001b\u202e"}`,
		`{"error":"ignored","code":"fabric_node_limit","limit":1.5,"current":0}`,
		`{"error":"ignored"} {"error":"` + testPollSecret + `"}`,
		`{"echo":"` + testPollSecret + `","code":"unknown_code"}`,
		`{"error":"` + strings.Repeat("x", maxResponseBytes) + `"}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(422)
			_, _ = io.WriteString(w, body)
		}))
		_, createErr := (Client{}).createEnrollRequest(context.Background(), srv.URL, testOrgID, "fake-csr", testLabels())
		_, _, pollErr := (Client{}).pollEnrollRequest(context.Background(), srv.URL, "req_1", testPollSecret)
		srv.Close()
		if createErr == nil || createErr.Error() != "enrollment request failed with HTTP 422" || pollErr == nil || pollErr.Error() != "poll failed with HTTP 422" {
			t.Fatalf("malformed body reflected instead of safe fallback: create=%v poll=%v", createErr, pollErr)
		}
	}
}

func TestApprovalDiagnosticResponsesKeepPollRetryPolicy(t *testing.T) {
	for _, status := range []int{402, 403, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var polls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == EnrollRequestsPath {
					w.WriteHeader(202)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"request_id": "req_1", "poll_secret": testPollSecret, "user_code": "ABCD", "poll_interval_s": 2,
					})
					return
				}
				polls.Add(1)
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": "Do not print " + testPollSecret, "code": "fabric_node_limit", "limit": 0, "current": 0,
				})
			}))
			defer srv.Close()
			var out bytes.Buffer
			_, err := (Client{}).EnrollByApproval(context.Background(), srv.URL, "fake-csr", testLabels(), opts(&out, &waitRecorder{}))
			if err == nil || strings.Contains(err.Error()+out.String(), testPollSecret) {
				t.Fatalf("error missing or secret escaped: %v", err)
			}
			wantPolls := int32(maxPollFailures)
			if status == 402 || status == 403 {
				wantPolls = 1
				if !strings.Contains(err.Error(), "fabric node limit reached (0 of 0)") {
					t.Fatalf("terminal body detail lost: %v", err)
				}
			}
			if polls.Load() != wantPolls {
				t.Fatalf("HTTP %d polled %d times, want %d", status, polls.Load(), wantPolls)
			}
		})
	}
}

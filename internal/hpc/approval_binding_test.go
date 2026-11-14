//go:build !windows

package hpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type approvalBindingTransport func(*http.Request) (*http.Response, error)

func (f approvalBindingTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type approvalCancelOnEOF struct {
	io.Reader
	cancel context.CancelFunc
}

func (r approvalCancelOnEOF) Read(b []byte) (int, error) {
	n, err := r.Reader.Read(b)
	if err == io.EOF {
		r.cancel()
	}
	return n, err
}

func bindingResponse(body any, status int) *http.Response {
	raw, _ := json.Marshal(body)
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}
}

func TestApprovalRequiresRequestedCanonicalOrgAndCompensates(t *testing.T) {
	for _, tc := range []struct {
		name, org                  string
		cleanupFails, cancelParent bool
	}{
		{name: "foreign", org: "3f3727b9-0000-4000-8000-000000000002"},
		{name: "noncanonical", org: strings.ToUpper(testOrgID)},
		{name: "cleanup-fails", org: "3f3727b9-0000-4000-8000-000000000002", cleanupFails: true},
		{name: "cancelled-parent", org: "3f3727b9-0000-4000-8000-000000000002", cancelParent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			deregisters := 0
			client := Client{HTTP: &http.Client{Transport: approvalBindingTransport(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host != "platform.example.test" {
					t.Fatal("credential was sent to response-supplied origin")
				}
				switch req.URL.Path {
				case EnrollRequestsPath:
					return bindingResponse(map[string]any{"request_id": "req_1", "poll_secret": testPollSecret, "user_code": "ABCD", "poll_interval_s": 2}, 202), nil
				case EnrollRequestsPath + "/req_1/poll":
					response := goodEnrollResponse("https://response-origin.example.test")
					response["org_id"] = tc.org
					resp := bindingResponse(response, 200)
					if tc.cancelParent {
						resp.Body = io.NopCloser(approvalCancelOnEOF{Reader: resp.Body, cancel: cancel})
					}
					return resp, nil
				case DeregisterPath:
					deregisters++
					if req.Context().Err() != nil {
						t.Fatal("cleanup inherited the cancelled parent")
					}
					deadline, ok := req.Context().Deadline()
					if !ok || time.Until(deadline) > 10*time.Second {
						t.Fatal("cleanup has no finite deadline")
					}
					if req.Header.Get("Authorization") != "Bearer dev_tok" {
						t.Fatal("cleanup used the wrong issued credential")
					}
					if tc.cleanupFails {
						return bindingResponse(map[string]string{}, 500), nil
					}
					return bindingResponse(map[string]any{"success": true, "node_id": testNodeID}, 200), nil
				default:
					t.Fatal("unexpected fake request")
					return nil, errors.New("unexpected request")
				}
			})}}
			resp, err := client.EnrollByApproval(ctx, "https://platform.example.test", "csr", testLabels(), opts(io.Discard, &waitRecorder{}))
			if err == nil || !strings.Contains(err.Error(), "organization mismatch") || deregisters != 1 {
				t.Fatalf("mismatch was not refused/compensated: err=%v deregisters=%d", err, deregisters)
			}
			if resp != (EnrollResponse{}) {
				t.Fatal("mismatched credentials escaped the approval client")
			}
			if errors.Is(err, ErrApprovalInterrupted) {
				t.Fatal("issued mismatch was misreported as a pre-issuance interruption")
			}
			if tc.cleanupFails && !strings.Contains(err.Error(), "remove it in the console") {
				t.Fatal("failed cleanup lost the operator recovery instruction")
			}
			if !tc.cleanupFails && !strings.Contains(err.Error(), "was deregistered") {
				t.Fatal("confirmed cleanup was not reported")
			}
		})
	}
}

func TestApprovalTimeoutBoundsBeforeAnyRequest(t *testing.T) {
	calls := 0
	c := Client{HTTP: &http.Client{Transport: approvalBindingTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("no request expected") })}}
	for _, timeout := range []time.Duration{-time.Nanosecond, MaxApprovalTimeout + time.Nanosecond} {
		o := opts(io.Discard, &waitRecorder{})
		o.Timeout = timeout
		if _, err := c.EnrollByApproval(context.Background(), "https://platform.example.test", "csr", testLabels(), o); err == nil || !strings.Contains(err.Error(), "--hpc-approval-timeout") {
			t.Fatal("invalid timeout accepted or not rejected at the shared boundary")
		}
	}
	if calls != 0 {
		t.Fatal("invalid timeout created an enrollment request")
	}
	for _, tc := range []struct{ input, want time.Duration }{{0, DefaultApprovalTimeout}, {time.Second, time.Second}, {MaxApprovalTimeout, MaxApprovalTimeout}} {
		got, err := NormalizeApprovalTimeout(tc.input)
		if err != nil || got != tc.want {
			t.Fatalf("timeout normalization: %v %v", got, err)
		}
	}
}

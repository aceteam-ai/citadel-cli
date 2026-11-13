//go:build !windows

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/hpc"
	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
)

const (
	approvalOrg    = "3f3727b9-0000-4000-8000-000000000001"
	approvalSecret = "pollsecret_do_not_leak_0123456789"
)

// newApprovalFake serves the create and poll routes; the poll answers
// pending `pending` times, then approves with a leaf for the CSR.
func newApprovalFake(t *testing.T, pending int32) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	var polls, tokenCalls atomic.Int32
	var csr atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case hpc.EnrollPath:
			tokenCalls.Add(1)
			var req map[string]any
			_ = json.Unmarshal(body, &req)
			leaf := certForCSR(t, req["csr_pem"].(string))
			_ = json.NewEncoder(w).Encode(map[string]any{"node_id": testHPCNodeID, "org_id": "org-1", "device_api_token": "dev_tok",
				"api_base_url": "http://" + r.Host, "leaf_pem": leaf, "chain_pem": leaf, "node_uid": "u", "expires_at": "2026-11-12T12:05:00Z"})
		case hpc.EnrollRequestsPath:
			var req map[string]any
			_ = json.Unmarshal(body, &req)
			csr.Store(req["csr_pem"])
			w.WriteHeader(202)
			_ = json.NewEncoder(w).Encode(map[string]any{"request_id": "req_1", "poll_secret": approvalSecret,
				"user_code": "WXYZ-9876", "expires_at": "2026-11-12T12:10:00Z", "poll_interval_s": 1})
		case hpc.EnrollRequestsPath + "/req_1/poll":
			if polls.Add(1) <= pending {
				w.WriteHeader(202)
				_, _ = w.Write([]byte(`{"status":"pending"}`))
				return
			}
			leaf := certForCSR(t, csr.Load().(string))
			_ = json.NewEncoder(w).Encode(map[string]any{"node_id": testHPCNodeID, "org_id": approvalOrg, "device_api_token": "dev_tok",
				"api_base_url": "http://" + r.Host, "leaf_pem": leaf, "chain_pem": leaf, "node_uid": "u", "expires_at": "2026-11-12T12:05:00Z"})
		case hpc.DeregisterPath:
			_, _ = w.Write([]byte(`{"success":true,"node_id":"` + testHPCNodeID + `"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &polls, &tokenCalls
}

func noWait(ctx context.Context, _ time.Duration) error { return ctx.Err() }

func TestEnrollHPCNodeByApproval(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("the hpc profile refuses root")
	}
	srv, polls, tokenCalls := newApprovalFake(t, 2)
	layout := testEnrollLayout(t)
	o := testEnrollOptions(t, srv.URL, "")
	var out bytes.Buffer
	o.OrgID, o.Out, o.Sleep = approvalOrg, &out, noWait
	creds, err := enrollHPCNode(context.Background(), layout, o)
	if err != nil {
		t.Fatal(err)
	}
	if creds.NodeID != testHPCNodeID || polls.Load() != 3 || tokenCalls.Load() != 0 {
		t.Fatalf("creds %+v polls %d token calls %d", creds, polls.Load(), tokenCalls.Load())
	}
	if !strings.Contains(out.String(), "code WXYZ-9876") || !strings.Contains(out.String(), "cluster testcluster") {
		t.Fatalf("output = %q", out.String())
	}
	if strings.Contains(out.String(), approvalSecret) {
		t.Fatal("output leaks the poll secret")
	}
	cfg, _ := os.ReadFile(hpcDeviceConfigPath(layout.NodeDir))
	if strings.Contains(string(cfg), approvalSecret) {
		t.Fatal("the poll secret reached disk")
	}
}

func TestEnrollHPCNodeTokenWinsOverOrg(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("the hpc profile refuses root")
	}
	srv, polls, tokenCalls := newApprovalFake(t, 0)
	o := testEnrollOptions(t, srv.URL, writeTestToken(t))
	o.OrgID, o.Sleep = approvalOrg, noWait
	if _, err := enrollHPCNode(context.Background(), testEnrollLayout(t), o); err != nil {
		t.Fatal(err)
	}
	if tokenCalls.Load() != 1 || polls.Load() != 0 {
		t.Fatalf("token calls %d polls %d", tokenCalls.Load(), polls.Load())
	}
}

func TestEnrollHPCNodeNeedsTokenOrValidOrg(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("the hpc profile refuses root")
	}
	srv, _, _ := newApprovalFake(t, 0)
	o := testEnrollOptions(t, srv.URL, "")
	if _, err := enrollHPCNode(context.Background(), testEnrollLayout(t), o); err == nil {
		t.Fatal("neither token nor org accepted")
	}
	o.OrgID = "nope"
	if _, err := enrollHPCNode(context.Background(), testEnrollLayout(t), o); err == nil || !strings.Contains(err.Error(), "UUID") {
		t.Fatalf("bad org: %v", err)
	}
}

func TestEnrollHPCNodeSignalDuringApprovalWait(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("the hpc profile refuses root")
	}
	srv, _, _ := newApprovalFake(t, 1000)
	layout := testEnrollLayout(t)
	ctx, cancel := context.WithCancel(context.Background())
	o := testEnrollOptions(t, srv.URL, "")
	o.OrgID = approvalOrg
	o.Out = io.Discard
	var n atomic.Int32
	o.Sleep = func(ctx context.Context, _ time.Duration) error {
		if n.Add(1) == 2 {
			cancel() // the signal handler cancels the context
		}
		return ctx.Err()
	}
	_, err := enrollHPCNode(ctx, layout, o)
	if err != hpc.ErrApprovalInterrupted {
		t.Fatalf("err = %v", err)
	}
	if _, serr := os.Stat(hpcDeviceConfigPath(layout.NodeDir)); !os.IsNotExist(serr) {
		t.Fatal("a cancelled wait left credentials behind")
	}
}

func TestEnrollHPCNodeApprovalTimeout(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("the hpc profile refuses root")
	}
	srv, _, _ := newApprovalFake(t, 1000)
	o := testEnrollOptions(t, srv.URL, "")
	o.OrgID, o.Out, o.ApprovalTimeout = approvalOrg, io.Discard, 50*time.Millisecond
	_, err := enrollHPCNode(context.Background(), testEnrollLayout(t), o)
	if err == nil || !strings.Contains(err.Error(), "no approval within") {
		t.Fatalf("err = %v", err)
	}
}

func TestHPCWorkerCommandOrgPath(t *testing.T) {
	spec := hpc.JobSpec{OrgID: approvalOrg, ApprovalTimeout: 10 * time.Minute, DrainTimeout: time.Minute, AuthService: "https://platform.example"}
	argv, _ := hpcWorkerCommand("/bin/citadel", spec, 8000, nil)
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--hpc-org "+approvalOrg) || !strings.Contains(joined, "--hpc-approval-timeout 10m0s") || strings.Contains(joined, "token-file") {
		t.Fatalf("argv = %v", argv)
	}
}

// This fake endpoint issues a valid leaf under a foreign org. No returned
// credential or certificate may reach the job-local device configuration.
func TestEnrollHPCNodeApprovalMismatchLeavesNoForeignCredentials(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("the hpc profile refuses root")
	}
	for _, cleanupStatus := range []int{http.StatusOK, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(cleanupStatus), func(t *testing.T) {
			var csr string
			var deregisters atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case hpc.EnrollRequestsPath:
					var req map[string]any
					_ = json.NewDecoder(r.Body).Decode(&req)
					csr, _ = req["csr_pem"].(string)
					w.WriteHeader(202)
					_ = json.NewEncoder(w).Encode(map[string]any{"request_id": "req_1", "poll_secret": approvalSecret, "user_code": "ABCD", "poll_interval_s": 2})
				case hpc.EnrollRequestsPath + "/req_1/poll":
					leaf := certForCSR(t, csr)
					_ = json.NewEncoder(w).Encode(map[string]any{"node_id": testHPCNodeID, "org_id": "3f3727b9-0000-4000-8000-000000000002", "device_api_token": "synthetic_foreign_token", "api_base_url": "http://" + r.Host, "leaf_pem": leaf, "chain_pem": leaf})
				case hpc.DeregisterPath:
					deregisters.Add(1)
					w.WriteHeader(cleanupStatus)
					_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "node_id": testHPCNodeID})
				default:
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			layout := testEnrollLayout(t)
			o := testEnrollOptions(t, srv.URL, "")
			o.OrgID = approvalOrg
			o.Out = io.Discard
			o.Sleep = noWait
			creds, err := enrollHPCNode(context.Background(), layout, o)
			if err == nil || !strings.Contains(err.Error(), "organization mismatch") || deregisters.Load() != 1 {
				t.Fatalf("foreign approval was not refused/compensated: %v", err)
			}
			if creds != (hpcCredentials{}) {
				t.Fatal("foreign credentials escaped enrollment")
			}
			store := nodeidentity.New(filepath.Join(layout.NodeDir, "identity"))
			for _, path := range []string{hpcDeviceConfigPath(layout.NodeDir), store.LeafPath(), store.CAChainPath()} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("foreign approval left credentials or certificates on disk")
				}
			}
			if cleanupStatus != http.StatusOK && !strings.Contains(err.Error(), "remove it in the console") {
				t.Fatal("cleanup failure lost operator recovery instruction")
			}
		})
	}
}

func TestHPCWorkSignalDoesNotMaskEnrollmentFailure(t *testing.T) {
	for _, enrollErr := range []error{errors.New("issued credential deregistration failed; remove it in the console"), errors.New("ordinary token enrollment failed"), hpc.ErrApprovalInterrupted} {
		t.Run(enrollErr.Error(), func(t *testing.T) {
			term := make(chan os.Signal, 1)
			term <- os.Interrupt
			creds, interrupted, err := enrollHPCWorkWithSignals(context.Background(), term, nil, func(ctx context.Context) (hpcCredentials, error) {
				select {
				case <-ctx.Done():
					return hpcCredentials{Token: "synthetic_partial_token"}, enrollErr
				case <-time.After(time.Second):
					return hpcCredentials{}, errors.New("signal did not reach enrollment")
				}
			})
			if creds != (hpcCredentials{}) {
				t.Fatal("failed enrollment returned partial credentials")
			}
			if enrollErr == hpc.ErrApprovalInterrupted {
				if !interrupted || err != nil {
					t.Fatal("explicit pre-issuance interruption did not exit cleanly")
				}
			} else if interrupted || err != enrollErr {
				t.Fatal("signal hid the real enrollment or cleanup failure")
			}
		})
	}
}

//go:build !windows

package hpc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testOrgID      = "3f3727b9-0000-4000-8000-000000000001"
	testPollSecret = "pollsecret_do_not_leak_0123456789"
)

type approvalServer struct {
	*httptest.Server
	mu       sync.Mutex
	polls    int
	script   []int // poll statuses, last one repeats
	createdB map[string]any
	interval int
	pollBody []string
}

func newApprovalServer(t *testing.T, script ...int) *approvalServer {
	a := &approvalServer{script: script, interval: 1}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.URL.Path == EnrollRequestsPath:
			_ = json.Unmarshal(body, &a.createdB)
			w.WriteHeader(202)
			_ = json.NewEncoder(w).Encode(map[string]any{"request_id": "req_1", "poll_secret": testPollSecret,
				"user_code": "ABCD-1234", "expires_at": "2026-11-12T12:10:00Z", "poll_interval_s": a.interval})
		case r.URL.Path == EnrollRequestsPath+"/req_1/poll":
			a.pollBody = append(a.pollBody, string(body))
			i := a.polls
			if i >= len(a.script) {
				i = len(a.script) - 1
			}
			a.polls++
			switch a.script[i] {
			case 202:
				w.WriteHeader(202)
				_, _ = w.Write([]byte(`{"status":"pending"}`))
			case 200:
				response := goodEnrollResponse("http://" + r.Host)
				response["org_id"] = testOrgID
				_ = json.NewEncoder(w).Encode(response)
			case 403:
				w.WriteHeader(403)
				_, _ = w.Write([]byte(`{"status":"denied","echo":"` + testPollSecret + `"}`))
			case 410:
				w.WriteHeader(410)
				_, _ = w.Write([]byte(`{"status":"expired"}`))
			default:
				w.WriteHeader(a.script[i])
			}
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(a.Close)
	return a
}

type waitRecorder struct {
	mu sync.Mutex
	d  []time.Duration
}

func (w *waitRecorder) sleep(ctx context.Context, d time.Duration) error {
	w.mu.Lock()
	w.d = append(w.d, d)
	w.mu.Unlock()
	return ctx.Err()
}

func opts(out io.Writer, rec *waitRecorder) ApprovalOptions {
	return ApprovalOptions{OrgID: testOrgID, Timeout: time.Minute, Out: out, Sleep: rec.sleep,
		Jitter: func(max time.Duration) time.Duration { return max / 2 }}
}

func TestEnrollByApprovalPendingPendingApproved(t *testing.T) {
	srv := newApprovalServer(t, 202, 202, 200)
	var out bytes.Buffer
	rec := &waitRecorder{}
	resp, err := Client{}.EnrollByApproval(context.Background(), srv.URL, "csr", testLabels(), opts(&out, rec))
	if err != nil {
		t.Fatal(err)
	}
	if resp.NodeID != testNodeID || srv.polls != 3 {
		t.Fatalf("resp %+v polls %d", resp, srv.polls)
	}
	if srv.createdB["org_id"] != testOrgID || srv.createdB["csr_pem"] != "csr" || srv.createdB["labels"] == nil || len(srv.createdB) != 3 {
		t.Fatalf("create body = %v", srv.createdB)
	}
	line := out.String()
	for _, want := range []string{"Waiting for approval: code ABCD-1234", "cluster c", "partition p", "account a", "job 1", "Approvals or Fabric"} {
		if !strings.Contains(line, want) {
			t.Errorf("output lacks %q: %q", want, line)
		}
	}
	for _, b := range srv.pollBody {
		if b != `{"poll_secret":"`+testPollSecret+`"}` {
			t.Errorf("poll body = %s", b)
		}
	}
}

func TestEnrollByApprovalIntervalAndJitterBounded(t *testing.T) {
	srv := newApprovalServer(t, 202, 202, 200)
	srv.interval = 1 // below the 2s floor
	rec := &waitRecorder{}
	if _, err := (Client{}).EnrollByApproval(context.Background(), srv.URL, "csr", testLabels(), opts(io.Discard, rec)); err != nil {
		t.Fatal(err)
	}
	for _, d := range rec.d {
		if d < 2*time.Second || d > 2400*time.Millisecond {
			t.Errorf("wait %v outside [2s, 2.4s]", d)
		}
	}
	if got := pollWait(1000, defaultJitter); got < maxPollInterval || got > maxPollInterval+maxPollInterval/5 {
		t.Errorf("cap: %v", got)
	}
	for i := 0; i < 200; i++ {
		if w := pollWait(10, defaultJitter); w < 10*time.Second || w >= 12*time.Second {
			t.Fatalf("jittered wait %v", w)
		}
	}
}

func TestEnrollByApprovalDeniedAndExpired(t *testing.T) {
	for status, want := range map[int]string{403: "denied", 410: "expired"} {
		srv := newApprovalServer(t, 202, status)
		var out bytes.Buffer
		_, err := Client{}.EnrollByApproval(context.Background(), srv.URL, "csr", testLabels(), opts(&out, &waitRecorder{}))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%d: %v", status, err)
		}
		if err != nil && strings.Contains(err.Error(), testPollSecret) {
			t.Error("error leaks the poll secret")
		}
	}
}

func TestEnrollByApprovalTimeout(t *testing.T) {
	srv := newApprovalServer(t, 202)
	o := ApprovalOptions{OrgID: testOrgID, Timeout: 100 * time.Millisecond, Out: io.Discard}
	start := time.Now()
	_, err := Client{}.EnrollByApproval(context.Background(), srv.URL, "csr", testLabels(), o)
	if err == nil || !strings.Contains(err.Error(), "no approval within") || errorsIs(err, ErrApprovalInterrupted) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("did not stop at the timeout")
	}
}

func errorsIs(err, target error) bool { return err == target }

func TestEnrollByApprovalCancelledDuringWait(t *testing.T) {
	srv := newApprovalServer(t, 202)
	ctx, cancel := context.WithCancel(context.Background())
	var polled atomic.Int32
	o := ApprovalOptions{OrgID: testOrgID, Timeout: time.Minute, Out: io.Discard,
		Sleep: func(ctx context.Context, d time.Duration) error {
			if polled.Add(1) == 2 {
				cancel() // SIGTERM arrives mid-wait
			}
			return ctx.Err()
		}}
	_, err := Client{}.EnrollByApproval(ctx, srv.URL, "csr", testLabels(), o)
	if err != ErrApprovalInterrupted {
		t.Fatalf("err = %v", err)
	}
}

func TestEnrollByApprovalSecretNeverEscapes(t *testing.T) {
	for _, script := range [][]int{{202, 200}, {403}, {410}, {400}} {
		srv := newApprovalServer(t, script...)
		var out bytes.Buffer
		_, err := Client{}.EnrollByApproval(context.Background(), srv.URL, "csr", testLabels(), opts(&out, &waitRecorder{}))
		all := out.String()
		if err != nil {
			all += err.Error()
		}
		if strings.Contains(all, testPollSecret) {
			t.Errorf("%v: secret leaked: %s", script, all)
		}
	}
}

func TestEnrollByApprovalTransientAndHardErrors(t *testing.T) {
	srv := newApprovalServer(t, 503, 503, 200)
	if _, err := (Client{}).EnrollByApproval(context.Background(), srv.URL, "csr", testLabels(), opts(io.Discard, &waitRecorder{})); err != nil {
		t.Fatalf("transient 503s should be retried: %v", err)
	}
	srv = newApprovalServer(t, 503)
	if _, err := (Client{}).EnrollByApproval(context.Background(), srv.URL, "csr", testLabels(), opts(io.Discard, &waitRecorder{})); err == nil {
		t.Fatal("endless 503 accepted")
	}
	srv = newApprovalServer(t, 404)
	if _, err := (Client{}).EnrollByApproval(context.Background(), srv.URL, "csr", testLabels(), opts(io.Discard, &waitRecorder{})); err == nil || srv.polls != 1 {
		t.Fatalf("404 should stop at once: %v polls %d", err, srv.polls)
	}
}

func TestEnrollByApprovalRefusesBadInputs(t *testing.T) {
	o := opts(io.Discard, &waitRecorder{})
	o.OrgID = "not-a-uuid"
	if _, err := (Client{}).EnrollByApproval(context.Background(), "http://127.0.0.1:1", "csr", testLabels(), o); err == nil {
		t.Fatal("bad org accepted")
	}
	if _, err := (Client{}).EnrollByApproval(context.Background(), "http://platform.example", "csr", testLabels(), opts(io.Discard, &waitRecorder{})); err == nil {
		t.Fatal("plain http to a non-loopback host accepted")
	}
}

func TestEnrollByApprovalDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	_, err := Client{HTTP: NewHTTPClient()}.EnrollByApproval(context.Background(), srv.URL, "csr", testLabels(), opts(io.Discard, &waitRecorder{}))
	if err == nil || hits.Load() != 0 {
		t.Fatalf("redirect followed or accepted: %v hits %d", err, hits.Load())
	}
}

func TestEnrollByApprovalCanonicalizesRequestedOrg(t *testing.T) {
	srv := newApprovalServer(t, 200)
	o := opts(io.Discard, &waitRecorder{})
	o.OrgID = strings.ToUpper(testOrgID)
	o.Timeout = 0
	resp, err := (Client{}).EnrollByApproval(context.Background(), srv.URL, "csr", testLabels(), o)
	if err != nil || resp.OrgID != testOrgID || srv.createdB["org_id"] != testOrgID {
		t.Fatalf("canonical request/default timeout failed: %v", err)
	}
}

package hpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Approve-on-arrival enrollment (aceteam#12717). The node posts an enroll
// request and polls until an org admin (or a pre-approval rule) approves it.
const (
	EnrollRequestsPath = "/api/fabric/hpc/enroll-requests"

	// DefaultApprovalTimeout is how long the node waits for an approval.
	DefaultApprovalTimeout = 10 * time.Minute
	// MaxApprovalTimeout bounds --hpc-approval-timeout.
	MaxApprovalTimeout = time.Hour

	minPollInterval = 2 * time.Second
	maxPollInterval = 60 * time.Second
	// pollJitterFraction is the largest extra wait added to each interval.
	pollJitterFraction = 0.2
	// maxPollFailures is how many consecutive transient poll failures end the
	// wait early.
	maxPollFailures = 5
)

var (
	orgIDPattern     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	userCodePattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
)

// ErrApprovalInterrupted means the wait was cancelled by a signal or the
// caller. Nothing was issued, so there is nothing to deregister.
var ErrApprovalInterrupted = errors.New("stopped while waiting for enrollment approval; nothing was enrolled")

// ValidateOrgID requires a UUID.
func ValidateOrgID(id string) error {
	if !orgIDPattern.MatchString(id) {
		return errors.New("--org must be an organization id (a UUID)")
	}
	return nil
}

// ApprovalOptions are the wait's edges; zero values are the production ones.
type ApprovalOptions struct {
	OrgID   string
	Timeout time.Duration
	// Out receives the one line that tells the admin what to approve.
	Out io.Writer
	// Sleep waits d or returns early with the context's error.
	Sleep func(ctx context.Context, d time.Duration) error
	// Jitter returns a value in [0, max).
	Jitter func(max time.Duration) time.Duration
}

func defaultSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func defaultJitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(max)))
}

// pollWait is the delay before the next poll: the server's interval, never
// under 2s or over 60s, plus bounded jitter.
func pollWait(intervalS int, jitter func(time.Duration) time.Duration) time.Duration {
	d := time.Duration(intervalS) * time.Second
	if d < minPollInterval {
		d = minPollInterval
	}
	if d > maxPollInterval {
		d = maxPollInterval
	}
	return d + jitter(time.Duration(float64(d)*pollJitterFraction))
}

type enrollRequestCreated struct {
	RequestID    string `json:"request_id"`
	PollSecret   string `json:"poll_secret"`
	UserCode     string `json:"user_code"`
	ExpiresAt    string `json:"expires_at"`
	PollInterval int    `json:"poll_interval_s"`
}

func readLimited(resp *http.Response) ([]byte, error) {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if len(data) > maxResponseBytes {
		return nil, errors.New("platform response is too large")
	}
	return data, nil
}

func (c Client) createEnrollRequest(ctx context.Context, baseURL, orgID, csrPEM string, labels Labels) (enrollRequestCreated, error) {
	body, err := json.Marshal(map[string]any{"org_id": orgID, "csr_pem": csrPEM, "labels": labels.Map()})
	if err != nil {
		return enrollRequestCreated{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+EnrollRequestsPath, bytes.NewReader(body))
	if err != nil {
		return enrollRequestCreated{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return enrollRequestCreated{}, fmt.Errorf("enrollment request failed: %w", scrubURLError(err))
	}
	defer resp.Body.Close()
	data, rerr := readLimited(resp)
	switch resp.StatusCode {
	case http.StatusAccepted:
	case http.StatusBadRequest:
		return enrollRequestCreated{}, errors.New("the platform rejected the enrollment request (check the Slurm labels and the CSR)")
	case http.StatusForbidden, http.StatusNotFound:
		return enrollRequestCreated{}, errors.New("the organization is unknown or does not allow HPC enrollment")
	case http.StatusTooManyRequests:
		return enrollRequestCreated{}, errors.New("the platform is rate limiting enrollment requests; try again shortly")
	default:
		return enrollRequestCreated{}, fmt.Errorf("enrollment request failed with HTTP %d", resp.StatusCode)
	}
	if rerr != nil {
		return enrollRequestCreated{}, rerr
	}
	var out enrollRequestCreated
	if err := json.Unmarshal(data, &out); err != nil {
		return enrollRequestCreated{}, errors.New("enrollment request response is not valid JSON")
	}
	if !requestIDPattern.MatchString(out.RequestID) || !userCodePattern.MatchString(out.UserCode) ||
		out.PollSecret == "" || len(out.PollSecret) > 512 {
		return enrollRequestCreated{}, errors.New("enrollment request response is malformed")
	}
	return out, nil
}

type pollOutcome int

const (
	pollPending pollOutcome = iota
	pollApproved
)

func (c Client) pollEnrollRequest(ctx context.Context, baseURL, requestID, secret string) (pollOutcome, EnrollResponse, error) {
	body, _ := json.Marshal(map[string]string{"poll_secret": secret})
	u := strings.TrimRight(baseURL, "/") + EnrollRequestsPath + "/" + url.PathEscape(requestID) + "/poll"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return pollPending, EnrollResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return pollPending, EnrollResponse{}, &transientPollError{fmt.Errorf("poll failed: %w", scrubURLError(err))}
	}
	defer resp.Body.Close()
	data, rerr := readLimited(resp)
	switch {
	case resp.StatusCode == http.StatusAccepted:
		return pollPending, EnrollResponse{}, nil
	case resp.StatusCode == http.StatusOK:
		if rerr != nil {
			return pollPending, EnrollResponse{}, rerr
		}
		var out EnrollResponse
		if err := json.Unmarshal(data, &out); err != nil {
			return pollPending, EnrollResponse{}, errors.New("enrollment response is not valid JSON")
		}
		if err := out.Validate(); err != nil {
			return pollPending, EnrollResponse{}, err
		}
		return pollApproved, out, nil
	case resp.StatusCode == http.StatusForbidden:
		return pollPending, EnrollResponse{}, errors.New("enrollment was denied by an organization admin")
	case resp.StatusCode == http.StatusGone:
		return pollPending, EnrollResponse{}, errors.New("the enrollment request expired before it was approved")
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return pollPending, EnrollResponse{}, &transientPollError{fmt.Errorf("poll failed with HTTP %d", resp.StatusCode)}
	default:
		return pollPending, EnrollResponse{}, fmt.Errorf("poll failed with HTTP %d", resp.StatusCode)
	}
}

type transientPollError struct{ err error }

func (e *transientPollError) Error() string { return e.err.Error() }
func (e *transientPollError) Unwrap() error { return e.err }

// WaitingLine is what the node prints so an admin can match the request.
func WaitingLine(userCode string, l Labels) string {
	return fmt.Sprintf("Waiting for approval: code %s, cluster %s, partition %s, account %s, job %s; approve in AceTeam under Approvals or Fabric",
		userCode, l.Cluster, l.Partition, l.Account, l.SlurmJobID)
}

// EnrollByApproval creates an enroll request and polls until it is approved,
// denied, expired, or the timeout passes. The poll secret lives only in this
// call's memory. The 200 body is validated exactly as the token route's is;
// the caller still verifies the leaf against its key.
func (c Client) EnrollByApproval(ctx context.Context, baseURL, csrPEM string, labels Labels, o ApprovalOptions) (EnrollResponse, error) {
	if err := checkPlatformURL(baseURL); err != nil {
		return EnrollResponse{}, fmt.Errorf("enrollment URL: %w", err)
	}
	if err := ValidateOrgID(o.OrgID); err != nil {
		return EnrollResponse{}, err
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultApprovalTimeout
	}
	if o.Sleep == nil {
		o.Sleep = defaultSleep
	}
	if o.Jitter == nil {
		o.Jitter = defaultJitter
	}
	waitCtx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	// timedOut distinguishes our deadline from the caller's cancellation.
	interrupted := func(err error) error {
		if ctx.Err() != nil {
			return ErrApprovalInterrupted
		}
		if waitCtx.Err() != nil {
			return fmt.Errorf("no approval within %s; the request was not approved in time", o.Timeout)
		}
		return err
	}

	created, err := c.createEnrollRequest(waitCtx, baseURL, strings.ToLower(o.OrgID), csrPEM, labels)
	if err != nil {
		return EnrollResponse{}, interrupted(err)
	}
	if o.Out != nil {
		fmt.Fprintln(o.Out, WaitingLine(created.UserCode, labels))
	}
	failures := 0
	for {
		if err := o.Sleep(waitCtx, pollWait(created.PollInterval, o.Jitter)); err != nil {
			return EnrollResponse{}, interrupted(err)
		}
		outcome, resp, err := c.pollEnrollRequest(waitCtx, baseURL, created.RequestID, created.PollSecret)
		if err != nil {
			if waitCtx.Err() != nil {
				return EnrollResponse{}, interrupted(err)
			}
			var tp *transientPollError
			if errors.As(err, &tp) {
				failures++
				if failures >= maxPollFailures {
					return EnrollResponse{}, fmt.Errorf("giving up after %d failed polls: %w", failures, tp.err)
				}
				continue
			}
			return EnrollResponse{}, err
		}
		failures = 0
		if outcome == pollApproved {
			return resp, nil
		}
	}
}

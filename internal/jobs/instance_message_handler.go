// internal/jobs/instance_message_handler.go
//
// Delivers a turn (a user/kickoff message) to a payload-launched BYOC agent
// instance running on THIS node (citadel-cli#462 / aceteam#5241).
//
// The instance's container is published on node-loopback only
// (127.0.0.1:<host_port>, by the #463 design) and is NOT reachable over the
// tailnet, so the backend cannot POST the turn to it directly. Instead the
// backend enqueues an INSTANCE_MESSAGE job on this node's per-node Redis stream
// (fail-closed, aceteam#4426) and this handler does the trivial loopback POST to
// the container's /hooks/agent ingress. This reuses the proven Redis dispatch
// path (same transport as SERVICE_START / SHELL_COMMAND) and avoids the
// mesh-HTTP/cert/SOCKS class of bugs entirely.
//
// The container's OUTBOUND reply (container -> POST /api/instances/{id}/reply)
// already works over normal egress and is unaffected by this handler; only the
// inbound hop is delivered here.
package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// InstanceMessageHandler resolves a running BYOC instance's published loopback
// port from the instance store and POSTs a turn to its /hooks/agent endpoint.
type InstanceMessageHandler struct {
	// instances is the registry of payload-launched instances (shared on-disk
	// state with the ServiceHandler). Lazily initialized; overridable in tests.
	instances *instanceStore
	// client is the HTTP client used for the loopback POST. Lazily initialized;
	// overridable in tests.
	client *http.Client
	// loopbackBaseURL builds the base URL (scheme://host:port) for a resolved
	// host port. Defaults to http://127.0.0.1:<port>; overridable in tests so
	// the POST can target an httptest.Server.
	loopbackBaseURL func(hostPort int) string
}

// NewInstanceMessageHandler creates a handler with production defaults.
func NewInstanceMessageHandler() *InstanceMessageHandler {
	return &InstanceMessageHandler{}
}

// hooksAgentURL builds the /hooks/agent turn-ingress URL for a published host
// port. Kept as a pure function so URL construction is unit-testable.
func hooksAgentURL(base string) string {
	return base + "/hooks/agent"
}

func defaultLoopbackBaseURL(hostPort int) string {
	return fmt.Sprintf("http://127.0.0.1:%d", hostPort)
}

func (h *InstanceMessageHandler) store() *instanceStore {
	if h.instances == nil {
		s, err := newInstanceStore()
		if err != nil {
			// Mirror ServiceHandler.instanceStore(): a store we cannot open
			// degrades to one rooted at an empty path; Get() returns not-found,
			// which fails closed (unknown instance) rather than mis-delivering.
			h.instances = &instanceStore{}
		} else {
			h.instances = s
		}
	}
	return h.instances
}

func (h *InstanceMessageHandler) httpClient() *http.Client {
	if h.client == nil {
		h.client = &http.Client{Timeout: 15 * time.Second}
	}
	return h.client
}

func (h *InstanceMessageHandler) baseURL(hostPort int) string {
	if h.loopbackBaseURL != nil {
		return h.loopbackBaseURL(hostPort)
	}
	return defaultLoopbackBaseURL(hostPort)
}

// instanceMessageResult is the JSON returned to the platform on success.
type instanceMessageResult struct {
	Delivered bool   `json:"delivered"`
	Service   string `json:"service"`
	Status    int    `json:"status"`
}

// Execute delivers a turn to a running BYOC instance on this node.
//
// Payload fields (the wire contract shared with the aceteam backend):
//   - service : the platform service name (instance store key, "ac-<shortcode>")
//   - message : the turn text to deliver
//   - name    : the message name/label (e.g. "Kickoff" / "Coordination")
//   - bearer  : the fully-derived /hooks/agent bearer ("hooks_<gateway_key>")
//
// Fail-closed: if the service is not a known running instance in THIS node's
// store, the turn is rejected rather than delivered to some other container.
func (h *InstanceMessageHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	service := job.Payload["service"]
	if service == "" {
		return nil, errors.New("job payload missing 'service' field")
	}
	message := job.Payload["message"]
	if message == "" {
		return nil, errors.New("job payload missing 'message' field")
	}
	bearer := job.Payload["bearer"]
	if bearer == "" {
		return nil, errors.New("job payload missing 'bearer' field")
	}
	name := job.Payload["name"]
	if name == "" {
		name = "Coordination"
	}

	// Resolve the instance's published loopback port from the local store. A
	// record exists only for instances this node launched and has not stopped,
	// so a hit is proof the target belongs here (fail-closed node scoping).
	rec, ok, err := h.store().Get(service)
	if err != nil {
		return nil, fmt.Errorf("failed to read instance store for %q: %w", service, err)
	}
	if !ok {
		return nil, fmt.Errorf("instance %q is not a known running instance on this node", service)
	}
	if rec.HostPort <= 0 {
		return nil, fmt.Errorf("instance %q has no published host port on record", service)
	}

	url := hooksAgentURL(h.baseURL(rec.HostPort))
	ctx.Log("info", "     - [Job %s] Delivering turn to instance %s at %s", job.ID, service, url)

	body, err := json.Marshal(map[string]string{"message": message, "name": name})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal turn body: %w", err)
	}

	status, err := h.postWithRetry(url, bearer, body)
	if err != nil {
		return nil, fmt.Errorf("failed to deliver turn to instance %q: %w", service, err)
	}
	if status >= 400 {
		return nil, fmt.Errorf("instance %q /hooks/agent returned status %d", service, status)
	}

	return json.Marshal(instanceMessageResult{Delivered: true, Service: service, Status: status})
}

// postWithRetry POSTs the turn, retrying a few times on connection-refused. The
// SERVICE_START that launched the container confirms docker "running" before
// this job is dispatched, but uvicorn inside the container may not have bound
// the port yet on a fast kickoff -- a bounded retry absorbs that race cheaply.
func (h *InstanceMessageHandler) postWithRetry(url, bearer string, body []byte) (int, error) {
	const attempts = 4
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		status, err := h.post(url, bearer, body)
		if err == nil {
			return status, nil
		}
		lastErr = err
		if !isConnRefused(err) {
			return 0, err
		}
	}
	return 0, lastErr
}

func (h *InstanceMessageHandler) post(url, bearer string, body []byte) (int, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.httpClient().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// Drain the body so the connection can be reused / closed cleanly.
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// isConnRefused reports whether err is a connection-refused dial error, the
// signature of a container whose HTTP server has not bound its port yet.
func isConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

// internal/jobs/extraction_handler.go
package jobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/services"
)

// ExtractionHandler proxies extraction requests to the local extraction service.
type ExtractionHandler struct{}

// extractionHostPort is the citadel-owned host port this handler talks to
// (services/ports.go). It is a package var rather than a direct reference to the
// const purely so tests can point it at an httptest listener; production never
// reassigns it.
var extractionHostPort = services.ExtractionHostPort

// extractionBaseURL is the host-local base URL for the extraction service, using
// the citadel-owned host port rather than a hardcoded literal.
func extractionBaseURL() string {
	return fmt.Sprintf("http://localhost:%d", extractionHostPort)
}

func (h *ExtractionHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	text, textOk := job.Payload["text"]
	schema, schemaOk := job.Payload["schema"]
	if !textOk {
		return nil, fmt.Errorf("job payload missing 'text' field")
	}

	ctx.Log("info", "     - [Job %s] Waiting for extraction service to become ready...", job.ID)
	if err := h.waitForReady(); err != nil {
		return nil, err
	}
	ctx.Log("info", "     - [Job %s] Extraction service is ready. Running extraction.", job.ID)

	// Build request payload
	requestPayload := map[string]any{
		"text": text,
	}
	if schemaOk && schema != "" {
		// Schema is JSON-encoded in the string payload
		var schemaObj any
		if err := json.Unmarshal([]byte(schema), &schemaObj); err != nil {
			return nil, fmt.Errorf("failed to parse 'schema' as JSON: %w", err)
		}
		requestPayload["schema"] = schemaObj
	}

	reqBody, err := json.Marshal(requestPayload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := http.Post(extractionBaseURL()+"/extract", "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to extraction service: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return bodyBytes, fmt.Errorf("extraction API returned non-200 status: %s", resp.Status)
	}

	return bodyBytes, nil
}

// Readiness bounds. maxWait is the tolerance for a service that IS there and
// still starting; notRunningGrace is the (much shorter) tolerance for one that
// has never accepted a connection at all.
//
// The split exists because those are different situations with different right
// answers, and collapsing them cost the caller 60s of silence either way
// (citadel-cli#653). A node that simply does not run the extraction service is
// the common case on a fabric where only some nodes carry the capability; making
// it wait out the full startup budget produces a timeout at the caller, which is
// the same signature as dead hardware. A few seconds is ample for a container
// that has already been started to bind its port -- the long budget is for model
// loading AFTER the port is up, which is what /health gates on.
// Vars, not consts, so tests can shrink them: exercising the real 8s/60s bounds
// would add ~20s to `go test ./...`, which scripts/release.sh gates on.
// TestExtractionReadinessDefaults pins the production values, so shrinking them
// in a test cannot quietly become the shipped behaviour.
var (
	extractionMaxWait        = 60 * time.Second
	extractionNotRunningWait = 8 * time.Second
	extractionPollInterval   = 1 * time.Second
	extractionDialTimeout    = 500 * time.Millisecond
)

// waitForReady blocks until the extraction service answers /health with 200, or
// returns an error naming WHICH failure occurred:
//
//   - nothing ever accepted a TCP connection -> the service is not running here
//     (a capability this node does not provide), reported after a short grace.
//   - the port answered but /health never returned 200 -> the service is present
//     but did not finish starting, reported after the full budget.
//
// Distinguishing the two is the whole point: the first is "this node cannot do
// that", the second is "this node is unwell". They read identically today.
func (h *ExtractionHandler) waitForReady() error {
	healthURL := extractionBaseURL() + "/health"
	addr := fmt.Sprintf("127.0.0.1:%d", extractionHostPort)
	startTime := time.Now()
	everAccepted := false

	for {
		resp, err := http.Get(healthURL)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			// A non-200 still proves something is listening and speaking HTTP.
			everAccepted = true
			resp.Body.Close()
		}
		if !everAccepted {
			// Probe the socket directly: an HTTP error does not distinguish
			// "connection refused" from "server returned garbage", and only the
			// former means the service is absent.
			if conn, dErr := net.DialTimeout("tcp", addr, extractionDialTimeout); dErr == nil {
				_ = conn.Close()
				everAccepted = true
			}
		}

		elapsed := time.Since(startTime)
		if !everAccepted && elapsed >= extractionNotRunningWait {
			return fmt.Errorf("extraction service is not running on this node: nothing accepted a connection on %s within %v "+
				"(start it with 'citadel service start extraction', or route this job to a node that provides the extraction capability)",
				addr, extractionNotRunningWait)
		}
		if elapsed >= extractionMaxWait {
			return fmt.Errorf("extraction service is listening on %s but did not report healthy within %v "+
				"(it is still starting, or wedged)", addr, extractionMaxWait)
		}
		time.Sleep(extractionPollInterval)
	}
}

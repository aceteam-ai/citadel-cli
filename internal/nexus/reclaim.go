// internal/nexus/reclaim.go
package nexus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ReclaimResult holds the outcome of a node reclamation attempt.
type ReclaimResult struct {
	// Reclaimed is true when a stale node was found and deleted.
	Reclaimed bool
	// Message describes what happened (for logging/display).
	Message string
}

// ReclaimStaleNode attempts to deregister a node with the given hostname
// so that a fresh registration can reuse the identity slot.
//
// This is designed for reboots where config is lost: the previous node (same
// hostname) goes stale in Headscale when the machine reboots and loses its
// tsnet state. Before registering again, we remove the old entry so the
// dashboard doesn't accumulate duplicate nodes.
//
// Safety: hostname collision between distinct machines is unlikely because
// getNodeName() replaces generic OS hostnames (debian, ubuntu, etc.) with
// citadel-<machine-id-prefix>, which is stable per hardware. If /etc/machine-id
// changes between boots (live ISO without persistence), the hostname differs
// and this call returns 404 (no-op). The deregister endpoint is org-scoped,
// so a caller can never affect nodes outside its own organization.
//
// The call is best-effort: if the backend returns 404 (no matching node),
// the result indicates nothing was reclaimed but no error is returned.
// Network/auth errors are returned so the caller can decide whether to
// proceed or abort.
func ReclaimStaleNode(ctx context.Context, baseURL, apiToken, hostname string) (*ReclaimResult, error) {
	if hostname == "" {
		return &ReclaimResult{Reclaimed: false, Message: "no hostname provided"}, nil
	}
	if apiToken == "" {
		return &ReclaimResult{Reclaimed: false, Message: "no API token available, skipping reclaim"}, nil
	}

	url := baseURL + "/api/fabric/device-auth/deregister"

	body, err := json.Marshal(DeregisterRequest{NodeName: hostname})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpClient := &http.Client{Timeout: 15 * time.Second}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiToken)

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to reclaim node '%s': %w", hostname, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		return &ReclaimResult{
			Reclaimed: true,
			Message:   fmt.Sprintf("reclaimed stale node '%s'", hostname),
		}, nil

	case resp.StatusCode == http.StatusNotFound:
		return &ReclaimResult{
			Reclaimed: false,
			Message:   fmt.Sprintf("no existing node '%s' found, nothing to reclaim", hostname),
		}, nil

	case resp.StatusCode == http.StatusUnauthorized:
		return &ReclaimResult{
			Reclaimed: false,
			Message:   "authentication failed, skipping reclaim",
		}, nil

	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("server returned status %d while reclaiming node '%s'", resp.StatusCode, hostname)

	default:
		return &ReclaimResult{Reclaimed: false, Message: "unexpected response"}, nil
	}
}

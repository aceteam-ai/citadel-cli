package redisapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// DesiredStatePathFormat is the device-authed binary endpoint the node GETs to
// PULL its control-plane-assigned DesiredState (aceteam#4273). The single %s is
// the node_id (Headscale hostname). It is EXPORTED so the paired backend serve
// endpoint (which does NOT exist yet) can match it exactly. The response body is
// raw octet-stream protobuf (fabric v1 DesiredState) — NOT base64-over-JSON —
// mirroring the ActualState report path (nodeStatePath).
const DesiredStatePathFormat = "/api/fabric/nodes/%s/desired-state"

// DesiredStatePath returns the desired-state fetch path for a node id.
func DesiredStatePath(nodeID string) string {
	return fmt.Sprintf(DesiredStatePathFormat, nodeID)
}

// GetDesiredState GETs the node's binary-protobuf DesiredState from the control
// plane, authenticated by the node's device API token. The body is returned raw
// (undecoded) so the caller (the reconcile ProtoProvider) owns the protobuf
// decode and keeps redisapi free of the fabric proto types. Auth mirrors
// PostNodeState exactly: Bearer <device token>, octet-stream content negotiation.
func (c *Client) GetDesiredState(ctx context.Context, nodeID string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+DesiredStatePath(nodeID), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create desired-state request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/octet-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("desired-state request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("desired-state fetch failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read desired-state response: %w", err)
	}
	return body, nil
}

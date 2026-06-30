package redisapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
)

// nodeStatePath is the device-authed binary endpoint that accepts a node's
// ActualState report (citadel#353). The server XADDs the decoded report to the
// node:state:stream Redis stream and a worker upserts it into relational
// columns. The endpoint requires the device_state:write scope (enforced
// server-side) and the node's device identity = its Headscale hostname.
const nodeStatePath = "/api/fabric/node-state"

// PostNodeState POSTs a binary-protobuf-encoded ActualState report to the
// control plane, authenticated by the node's device API token. Unlike the JSON
// helpers (doRequest), the body is raw octet-stream — the report rides as
// binary protobuf, NOT base64-over-JSON.
func (c *Client) PostNodeState(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+nodeStatePath, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create node-state request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("node-state request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("node-state post failed with status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

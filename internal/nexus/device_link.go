package nexus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
)

var enrollmentNodeIDPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

// DeviceLinkRefusedError is a definite answer from the link route: a 4xx, or a
// 200 that does not confirm the link. Transport failures, timeouts and 5xx are
// not refusals; they say nothing about whether this enrollment can be linked.
type DeviceLinkRefusedError struct {
	StatusCode int
}

func (e *DeviceLinkRefusedError) Error() string {
	if e.StatusCode == http.StatusOK {
		return "device link did not confirm enrollment"
	}
	return fmt.Sprintf("device link refused (HTTP %d); enrollment is incomplete", e.StatusCode)
}

// LinkRegisteredNode completes the current device grant after registration.
// The server verifies the used registration key against its reservation; neither
// a hostname nor an IP address is an enrollment proof. The request must never
// expose its grant in diagnostics or forward it on redirects. The public link
// route authorizes the grant plus reserved registration proof, not a bearer.
func (c *DeviceAuthClient) LinkRegisteredNode(ctx context.Context, deviceCode, nodeID string) error {
	if deviceCode == "" || !enrollmentNodeIDPattern.MatchString(nodeID) {
		return fmt.Errorf("device link requires the current grant and registered node ID")
	}
	body, _ := json.Marshal(struct {
		NodeID string `json:"nodeId"`
	}{nodeID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/fabric/device-config/"+url.PathEscape(deviceCode)+"/link", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("could not prepare device link request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Transport errors can contain the URL (and therefore the grant).
		return fmt.Errorf("device link request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return &DeviceLinkRefusedError{StatusCode: resp.StatusCode}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("device link unavailable (HTTP %d)", resp.StatusCode)
	}
	var result struct {
		Success *bool `json:"success"`
	}
	if err := decodeBoundedDeviceAuthJSON(resp.Body, &result, "device link response"); err != nil || result.Success == nil {
		return fmt.Errorf("device link response was not understood")
	}
	if !*result.Success {
		return &DeviceLinkRefusedError{StatusCode: http.StatusOK}
	}
	return nil
}

// ServerVersion reads the auth service's release version from /api/version.
// Callers use it only to interpret a link refusal; an unreadable version is
// reported as an error, never guessed.
func (c *DeviceAuthClient) ServerVersion(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/version", nil)
	if err != nil {
		return "", fmt.Errorf("could not prepare server version request")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("server version request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("server version unavailable (HTTP %d)", resp.StatusCode)
	}
	var result struct {
		Version string `json:"version"`
	}
	if err := decodeBoundedDeviceAuthJSON(resp.Body, &result, "server version response"); err != nil {
		return "", err
	}
	if result.Version == "" {
		return "", fmt.Errorf("server version response has no version")
	}
	return result.Version, nil
}

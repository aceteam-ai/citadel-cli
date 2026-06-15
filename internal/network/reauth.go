// internal/network/reauth.go
// Helper for fetching a fresh Headscale authkey from the platform API.
package network

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// authkeyResponse is the JSON body returned by POST /api/fabric/authkey/generate.
type authkeyResponse struct {
	Authkey   string `json:"authkey"`
	ExpiresIn int    `json:"expires_in"`
	Message   string `json:"message,omitempty"`
}

// FetchFreshAuthkey requests a new Headscale preauth key from the platform
// using the device API token (act_*). The token authenticates the device
// and the platform generates a single-use key scoped to the device's org.
//
// PREREQUISITE: The device API token must have /api/fabric/authkey/generate
// in its allowedEndpoints list. Current device tokens (created by the
// device-auth approve flow) are scoped to /api/fabric/redis/** and
// /api/fabric/worker-config only — this endpoint must be added to the
// allowlist in utils/deviceApiKeys.ts for auto-heal to work.
// See: https://github.com/aceteam-ai/aceteam/issues/175
//
// Returns the preauth key string or an error.
func FetchFreshAuthkey(ctx context.Context, apiBaseURL, deviceAPIToken string) (string, error) {
	if apiBaseURL == "" {
		return "", fmt.Errorf("api_base_url is empty")
	}
	if deviceAPIToken == "" {
		return "", fmt.Errorf("device_api_token is empty")
	}

	url := apiBaseURL + "/api/fabric/authkey/generate"

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+deviceAPIToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("authkey generate returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result authkeyResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	if result.Authkey == "" {
		return "", fmt.Errorf("response contained empty authkey")
	}

	return result.Authkey, nil
}

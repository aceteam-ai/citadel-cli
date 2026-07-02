// internal/network/reauth.go
// Helper for fetching a fresh Headscale authkey from the platform API.
package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// authkeyResponse is the JSON body returned by POST /api/fabric/authkey/generate.
type authkeyResponse struct {
	Authkey   string `json:"authkey"`
	ExpiresIn int    `json:"expires_in"`
	Message   string `json:"message,omitempty"`
}

// ErrAuthkeyScope is returned when the platform rejects a fresh-authkey request
// because the device token lacks the device_authkey:write scope. A token minted
// before that scope was granted (aceteam #4432) hits this. It is a recoverable,
// operator-actionable condition (re-run 'citadel init'), NOT a broken node — a
// persistent (non-ephemeral, backend PR #4584) node still self-heals via the
// no-authkey reconnect on restart. Callers should surface the remedy rather than
// treat it as fatal. Wrapped so errors.Is works.
var ErrAuthkeyScope = errors.New("device token lacks device_authkey:write scope")

// IsAuthkeyScopeError reports whether err is (or wraps) the authkey-scope
// rejection, so callers (e.g. the online node-key renewer) can distinguish the
// "old token, re-init to enable self-renewal" case from generic failures.
func IsAuthkeyScopeError(err error) bool {
	return errors.Is(err, ErrAuthkeyScope)
}

// FetchFreshAuthkey requests a new Headscale preauth key from the platform
// using the device API token (act_*). The token authenticates the device
// and the platform generates a single-use key scoped to the device's org.
//
// PREREQUISITE: The device API token must carry the device_authkey:write scope
// and have /api/fabric/authkey/generate in its allowedEndpoints list. Tokens
// minted by the device-auth approve flow have carried both since aceteam #4432.
// A token minted BEFORE that grant is rejected with HTTP 403; the token is not
// regenerated on citadel upgrade, only by re-running 'citadel init'. That case
// is returned as ErrAuthkeyScope so callers can surface the exact remedy rather
// than treat a recoverable old-token node as broken.
// See: https://github.com/aceteam-ai/aceteam/issues/175 (original grant),
//
//	https://github.com/aceteam-ai/aceteam/pull/4584 (durable-key baseline).
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
		// Classify the network error for clearer diagnostics
		if isConnectivityError(err) {
			return "", fmt.Errorf("cannot reach API at %s — check network connectivity: %w", apiBaseURL, err)
		}
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// A 403 here is most often the missing device_authkey:write scope on an
		// old device token (minted before aceteam #4432). Classify it so callers
		// (the online node-key renewer, reconnect) can tell the operator the exact
		// remedy: re-run 'citadel init'. A persistent node still self-heals on
		// restart via the no-authkey reconnect, so this is not a broken node.
		if resp.StatusCode == http.StatusForbidden {
			return "", fmt.Errorf("%w (HTTP 403) — re-run 'citadel init' to re-authenticate with the "+
				"self-renewal scope; this node still reconnects on restart: %s", ErrAuthkeyScope, string(body))
		}
		return "", fmt.Errorf("device API token rejected (HTTP %d) — re-run 'citadel init' to re-authenticate: %s", resp.StatusCode, string(body))
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

// isConnectivityError returns true if the error indicates a network
// connectivity problem (DNS failure, connection refused, timeout).
func isConnectivityError(err error) bool {
	if err == nil {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "network is unreachable") ||
		errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "i/o timeout")
}

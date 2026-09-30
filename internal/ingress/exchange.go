package ingress

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const exchangeResponseMaxBytes = 64 << 10

// ExchangeResult is the successful app-host credential handoff returned by the
// control plane. MaxAge is expressed in seconds, matching both the wire field
// and http.Cookie.MaxAge.
type ExchangeResult struct {
	Credential string `json:"credential"`
	MaxAge     int    `json:"max_age"`
	Next       string `json:"next"`
}

// Exchanger redeems a one-time browser handoff code. The returned status is the
// raw HTTP status from the control plane: callers can render a static failure
// for 4xx and make 5xx retryable. Transport and malformed-response failures
// return an error; status is zero when no HTTP response was received.
type Exchanger interface {
	Exchange(ctx context.Context, slug, code, nonce string) (ExchangeResult, int, error)
}

type httpExchanger struct {
	exchangeURL string
	token       string
	client      *http.Client
}

type exchangeRequest struct {
	Slug  string `json:"slug"`
	Code  string `json:"code"`
	Nonce string `json:"nonce"`
}

// NewHTTPExchanger builds an Exchanger over the full control-plane exchange
// endpoint. The supplied client is cloned so its redirect policy is never
// mutated; exchange requests always stop at the first redirect response.
func NewHTTPExchanger(exchangeURL, token string, client *http.Client) Exchanger {
	exchangeClient := clientWithoutRedirects(client, 10*time.Second)
	// The handoff contract is a hard ten-second ceiling, including when callers
	// supply a shared client with no timeout or a more permissive one. Preserve a
	// shorter caller timeout, but never let exchange traffic wait longer.
	if exchangeClient.Timeout <= 0 || exchangeClient.Timeout > 10*time.Second {
		exchangeClient.Timeout = 10 * time.Second
	}
	return &httpExchanger{
		exchangeURL: exchangeURL,
		token:       token,
		client:      exchangeClient,
	}
}

func (e *httpExchanger) Exchange(ctx context.Context, slug, code, nonce string) (ExchangeResult, int, error) {
	payload, err := json.Marshal(exchangeRequest{Slug: slug, Code: code, Nonce: nonce})
	if err != nil {
		return ExchangeResult{}, 0, fmt.Errorf("exchange: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.exchangeURL, bytes.NewReader(payload))
	if err != nil {
		return ExchangeResult{}, 0, fmt.Errorf("exchange: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return ExchangeResult{}, 0, fmt.Errorf("exchange: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= http.StatusBadRequest {
			return ExchangeResult{}, resp.StatusCode, nil
		}
		return ExchangeResult{}, resp.StatusCode, fmt.Errorf("exchange: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, exchangeResponseMaxBytes+1))
	if err != nil {
		return ExchangeResult{}, resp.StatusCode, fmt.Errorf("exchange: read response: %w", err)
	}
	if len(body) > exchangeResponseMaxBytes {
		return ExchangeResult{}, resp.StatusCode, fmt.Errorf("exchange: response exceeds %d-byte limit", exchangeResponseMaxBytes)
	}
	var result ExchangeResult
	if err := json.Unmarshal(body, &result); err != nil {
		return ExchangeResult{}, resp.StatusCode, fmt.Errorf("exchange: decode response: %w", err)
	}
	if result.Credential == "" {
		return ExchangeResult{}, resp.StatusCode, fmt.Errorf("exchange: response credential is empty")
	}
	if result.MaxAge <= 0 {
		return ExchangeResult{}, resp.StatusCode, fmt.Errorf("exchange: response max_age must be positive")
	}
	if result.Next == "" {
		return ExchangeResult{}, resp.StatusCode, fmt.Errorf("exchange: response next is empty")
	}
	return result, resp.StatusCode, nil
}

// DeriveExchangeURL turns a routes endpoint URL into its sibling exchange URL
// (routesURL/../exchange), matching DeriveAuthzURL's generic convention.
func DeriveExchangeURL(routesURL string) (string, error) {
	u, err := url.Parse(routesURL)
	if err != nil {
		return "", err
	}
	trimmed := strings.TrimRight(u.Path, "/")
	idx := strings.LastIndex(trimmed, "/")
	if idx < 0 {
		u.Path = "/exchange"
	} else {
		u.Path = trimmed[:idx] + "/exchange"
	}
	return u.String(), nil
}

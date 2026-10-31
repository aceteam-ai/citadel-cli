// Package cdpdiscovery resolves a local Chrome page without granting the
// discovery document authority to redirect a probe or choose another host.
package cdpdiscovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Chrome pretty-prints replies and may include many ad iframe targets with
// multi-KiB URLs. The same reviewed ceiling applies to every discovery caller.
const MaxResponseBytes = 512 << 10
const MaxPageTargets = 64
const ProbeTimeout = 5 * time.Second

type Target struct {
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// Discover validates the entire bounded response before exposing its first
// page. The debugger authority is always pinned to the probed loopback port;
// the page URL is retained for meeting sign-in detection, not used for dialing.
func Discover(ctx context.Context, debugPort int, client *http.Client) (Target, error) {
	if debugPort < 1 || debugPort > 65535 || client == nil {
		return Target{}, fmt.Errorf("CDP target discovery requires a valid local port and client")
	}
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	probe := *client
	probe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json", debugPort), nil)
	if err != nil {
		return Target{}, safeFailure(ctx, "CDP target discovery request failed", err)
	}
	response, err := probe.Do(request)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		return Target{}, safeFailure(ctx, "CDP target discovery request failed", err)
	}
	if response == nil || response.Body == nil || response.StatusCode != http.StatusOK {
		return Target{}, fmt.Errorf("CDP target discovery requires HTTP 200")
	}
	if response.ContentLength > MaxResponseBytes {
		return Target{}, fmt.Errorf("CDP target discovery exceeds 512 KiB")
	}
	// Byte admission precedes JSON parsing, including malformed oversized input.
	// This is a fixed-size response buffer, never an unbounded target slice.
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if err != nil {
		return Target{}, safeFailure(ctx, "CDP target discovery body read failed", err)
	}
	if len(raw) > MaxResponseBytes {
		return Target{}, fmt.Errorf("CDP target discovery exceeds 512 KiB")
	}
	target, err := decode(raw, debugPort)
	if ctx.Err() != nil {
		return Target{}, safeFailure(ctx, "CDP target discovery cancelled", ctx.Err())
	}
	return target, err
}

// Preserve cancellation classification without exposing the transport's URL,
// body, or diagnostic string. The only wrapped causes are standard sentinels.
func safeFailure(ctx context.Context, label string, cause error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", label, ctx.Err())
	}
	if errors.Is(cause, context.Canceled) {
		return fmt.Errorf("%s: %w", label, context.Canceled)
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", label, context.DeadlineExceeded)
	}
	return errors.New(label)
}

func decode(raw []byte, debugPort int) (Target, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('[') {
		return Target{}, fmt.Errorf("CDP target discovery requires a JSON array")
	}
	var selected Target
	pages := 0
	for decoder.More() {
		var target Target
		if err := decoder.Decode(&target); err != nil {
			return Target{}, fmt.Errorf("CDP target discovery contains invalid target JSON")
		}
		if target.Type != "page" {
			continue
		}
		pages++
		if pages > MaxPageTargets {
			return Target{}, fmt.Errorf("CDP target discovery exceeds 64 page targets")
		}
		if selected.WebSocketDebuggerURL == "" && target.WebSocketDebuggerURL != "" {
			selected = target
		}
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim(']') {
		return Target{}, fmt.Errorf("CDP target discovery has an incomplete array")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return Target{}, fmt.Errorf("CDP target discovery has trailing JSON")
	}
	if selected.WebSocketDebuggerURL == "" {
		return Target{}, fmt.Errorf("no page target found on CDP port %d", debugPort)
	}
	endpoint, err := url.Parse(selected.WebSocketDebuggerURL)
	if err != nil || endpoint == nil || (endpoint.Scheme != "ws" && endpoint.Scheme != "wss") || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.Fragment != "" {
		return Target{}, fmt.Errorf("CDP target discovery returned an invalid debugger URL")
	}
	endpoint.Scheme = "ws"
	endpoint.Host = "127.0.0.1:" + strconv.Itoa(debugPort)
	selected.WebSocketDebuggerURL = endpoint.String()
	return selected, nil
}

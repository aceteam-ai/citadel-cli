// Package notify lets a Citadel node emit a live notification to its org —
// the node authenticates with its device API token (org-scoped, exactly like
// every other node→backend call) and POSTs a notification to the AceTeam
// backend, which relays it to the org user's registered iOS device(s) via APNs.
//
// This powers the "agent on this machine wants your attention / HITL approval"
// flow (aceteam-ai/aceteam#4144, #4145): an agent running on the node can ask
// for a human's attention on their phone.
//
// # Assumed backend contract (reconcile with aceteam-ai/aceteam#4219)
//
// As of this writing the receiving half (#4219) is being built in parallel and
// the exact route is not yet on main. This client implements the most-likely
// contract, grounded in the already-merged APNs send path (aceteam#3989):
//
//	POST {api_base_url}/api/fabric/notify
//	Authorization: Bearer <device_api_token>
//	X-Fabric-Source: citadel-cli
//	Content-Type: application/json
//
//	{
//	  "title": "<string, required>",
//	  "body":  "<string, required>",
//	  "target": "chat" | "nodes" | "terminal" | "settings",   // optional, default "chat"
//	  "conversation_id": "<uuid>"                              // optional deep-link target
//	}
//
//	-> 2xx { "success": true }   (notification accepted / queued for delivery)
//
// Field names mirror aceteam#3989's APNs payload builder
// (build_apns_payload → {aps:{alert:{title,body}}, target, conversationId}).
// The org is NOT sent in the body: it is derived server-side from the
// device API token (the same withAuthenticatedContext → session_org_id path
// used by /api/machines/[machineId]/manage/* node routes).
//
// If #4219 finalizes a different route or field names, only the constants and
// the Notification JSON tags below need to change; the auth + send flow stays.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultNotifyPath is the assumed backend route (see package doc / aceteam#4219).
const defaultNotifyPath = "/api/fabric/notify"

// fabricSourceHeader marks the request as originating from the Citadel CLI,
// matching the convention used by the remote-logs node route.
const (
	headerFabricSource = "X-Fabric-Source"
	fabricSourceValue  = "citadel-cli"
)

// Target identifies which iOS surface a notification should deep-link into.
// Values mirror the APNs payload "target" enum from aceteam#3989.
type Target string

const (
	TargetChat     Target = "chat"
	TargetNodes    Target = "nodes"
	TargetTerminal Target = "terminal"
	TargetSettings Target = "settings"
)

// Notification is the request body sent to the backend. Org scope is carried
// by the auth token, not this struct (see package doc).
type Notification struct {
	// Title is the bold first line of the push. Required.
	Title string `json:"title"`
	// Body is the notification message. Required.
	Body string `json:"body"`
	// Target is the iOS surface to deep-link into. Optional; defaults to "chat".
	Target Target `json:"target,omitempty"`
	// ConversationID deep-links the notification to a specific chat/conversation.
	// Optional.
	ConversationID string `json:"conversation_id,omitempty"`
}

// Result is what the caller learns about a send. Because actual APNs delivery
// is unobservable from the node, Accepted means only that the backend received
// and queued the notification — not that a phone displayed it.
type Result struct {
	// Accepted is true when the backend returned a 2xx status.
	Accepted bool
	// StatusCode is the raw HTTP status returned by the backend.
	StatusCode int
}

// Client sends authenticated, org-scoped notifications to the AceTeam backend.
type Client struct {
	baseURL    string
	token      string
	path       string
	httpClient *http.Client
}

// Config configures a notify Client.
type Config struct {
	// BaseURL is the AceTeam API base URL (e.g. "https://aceteam.ai").
	// Typically deviceConfig.APIBaseURL, falling back to the auth-service URL.
	BaseURL string
	// Token is the device_api_token from device authentication. Required.
	Token string
	// Path overrides the notify endpoint path (defaults to defaultNotifyPath).
	// Exposed so it can be repointed without a code change once #4219 lands.
	Path string
	// HTTPClient overrides the HTTP client (mainly for tests). Optional.
	HTTPClient *http.Client
}

// NewClient builds a notify Client from cfg.
func NewClient(cfg Config) *Client {
	path := cfg.Path
	if path == "" {
		path = defaultNotifyPath
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		token:      cfg.Token,
		path:       path,
		httpClient: hc,
	}
}

// Send delivers a notification to the org via the backend. It returns a Result
// describing whether the backend accepted the notification (a 2xx). A nil
// error with Result.Accepted == true means the backend queued the push; the
// node cannot observe whether the phone actually displayed it.
func (c *Client) Send(ctx context.Context, n Notification) (*Result, error) {
	if c.token == "" {
		return nil, fmt.Errorf("notify: missing device API token (run 'citadel init' or 'citadel login')")
	}
	if c.baseURL == "" {
		return nil, fmt.Errorf("notify: missing API base URL")
	}
	if strings.TrimSpace(n.Title) == "" {
		return nil, fmt.Errorf("notify: title is required")
	}
	if strings.TrimSpace(n.Body) == "" {
		return nil, fmt.Errorf("notify: body is required")
	}

	payload, err := json.Marshal(n)
	if err != nil {
		return nil, fmt.Errorf("notify: marshal request: %w", err)
	}

	url := c.baseURL + c.path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("notify: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set(headerFabricSource, fabricSourceValue)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("notify: send request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return &Result{Accepted: true, StatusCode: resp.StatusCode}, nil
	}

	return &Result{Accepted: false, StatusCode: resp.StatusCode},
		fmt.Errorf("notify: backend returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

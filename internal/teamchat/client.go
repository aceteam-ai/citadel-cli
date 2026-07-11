package teamchat

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
	"strings"
	"time"
)

// APIError is a non-2xx response from the Team Chat API. It preserves the
// HTTP status so callers can branch on auth failures (401/403) versus
// transient server errors.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("team chat API error (status %d): %s", e.StatusCode, e.Message)
}

// IsAuthError reports whether err is an APIError with a 401 or 403 status —
// i.e. the credential is missing, invalid, or lacks the scope for Team Chat.
// A device API token currently always yields 403 here because its endpoint
// whitelist excludes /api/channels/** (see aceteam-ai/citadel-cli#495); the
// TUI turns this into an actionable "configure an API key" message.
func IsAuthError(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden
	}
	return false
}

// Client is a typed HTTP client for the AceTeam Team Chat REST API.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// ClientConfig configures a Client.
type ClientConfig struct {
	// BaseURL is the AceTeam API base URL (e.g. "https://aceteam.ai").
	BaseURL string
	// Token is the Bearer credential: a user act_ API key or a Supabase JWT.
	// A device_api_token authenticates but is scope-denied on these routes
	// today (see IsAuthError).
	Token string
	// Timeout is the per-request HTTP timeout (default 15s).
	Timeout time.Duration
}

// NewClient creates a Team Chat API client.
func NewClient(cfg ClientConfig) *Client {
	if cfg.Timeout == 0 {
		cfg.Timeout = 15 * time.Second
	}
	return &Client{
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		token:      cfg.Token,
		httpClient: &http.Client{Timeout: cfg.Timeout},
	}
}

// BaseURL returns the configured API base URL.
func (c *Client) BaseURL() string { return c.baseURL }

// HasToken reports whether a credential is configured.
func (c *Client) HasToken() bool { return c.token != "" }

// MessagesOptions bounds a Messages call. Zero values use server defaults
// (limit 50, direction "before" = newest page, chronological order).
type MessagesOptions struct {
	// Limit is the page size (1-100; server default 50).
	Limit int
	// Cursor is a message ID to page from, per Direction.
	Cursor string
	// Direction is "before" (older than cursor; default) or "after".
	Direction string
}

// ListChannels returns the channels visible to the authenticated user's org.
// GET /api/channels — the server auto-seeds #general for empty orgs.
func (c *Client) ListChannels(ctx context.Context) ([]Channel, error) {
	var out struct {
		Channels []Channel `json:"channels"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/channels", nil, &out); err != nil {
		return nil, err
	}
	return out.Channels, nil
}

// Messages returns a page of messages for a channel.
// GET /api/channels/{id}/messages
func (c *Client) Messages(ctx context.Context, channelID string, opts MessagesOptions) (MessagesPage, error) {
	q := url.Values{}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	if opts.Direction != "" {
		q.Set("direction", opts.Direction)
	}
	path := "/api/channels/" + url.PathEscape(channelID) + "/messages"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}

	var out MessagesPage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return MessagesPage{}, err
	}
	return out, nil
}

// SendMessage posts a message to a channel as the authenticated user.
// POST /api/channels/{id}/messages. parentMessageID, when non-empty, makes
// the message a threaded reply.
func (c *Client) SendMessage(ctx context.Context, channelID, content, parentMessageID string) (Message, error) {
	body := map[string]any{"content": content}
	if parentMessageID != "" {
		body["parent_message_id"] = parentMessageID
	}

	var out struct {
		Message Message `json:"message"`
	}
	path := "/api/channels/" + url.PathEscape(channelID) + "/messages"
	if err := c.doJSON(ctx, http.MethodPost, path, body, &out); err != nil {
		return Message{}, err
	}
	return out.Message, nil
}

// ListMembers returns the members of a channel (direct + team-derived).
// GET /api/channels/{id}/members
func (c *Client) ListMembers(ctx context.Context, channelID string) ([]Member, error) {
	var out struct {
		Members []Member `json:"members"`
	}
	path := "/api/channels/" + url.PathEscape(channelID) + "/members"
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Members, nil
}

// SearchMessages searches message content within a channel.
// GET /api/channels/{id}/messages/search
func (c *Client) SearchMessages(ctx context.Context, channelID, query string, limit int) ([]Message, error) {
	q := url.Values{}
	q.Set("q", query)
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/api/channels/" + url.PathEscape(channelID) + "/messages/search?" + q.Encode()

	var out struct {
		Messages []Message `json:"messages"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Messages, nil
}

// MarkRead advances the caller's read cursor for a channel to a message.
// POST /api/channels/{id}/mark-read
func (c *Client) MarkRead(ctx context.Context, channelID, messageID string) error {
	path := "/api/channels/" + url.PathEscape(channelID) + "/mark-read"
	return c.doJSON(ctx, http.MethodPost, path, map[string]any{"messageId": messageID}, nil)
}

// UnreadCounts returns per-channel unread message counts for the caller.
// GET /api/channels/unread-counts
func (c *Client) UnreadCounts(ctx context.Context) (map[string]int, error) {
	var out struct {
		Counts map[string]int `json:"counts"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/channels/unread-counts", nil, &out); err != nil {
		return nil, err
	}
	return out.Counts, nil
}

// doJSON performs an authenticated JSON request. A nil body sends no payload;
// a nil out discards the response body. Non-2xx responses are returned as
// *APIError with the server's error message when parseable.
func (c *Client) doJSON(ctx context.Context, method, path string, body any, out any) error {
	var reqBody io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	// Bound reads so a misbehaving server can't exhaust memory.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{StatusCode: resp.StatusCode, Message: parseErrorMessage(data)}
	}

	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// parseErrorMessage extracts a human-readable message from an error response
// body. The Next.js routes return {"error": "..."}; scope denials may also
// carry {"message": "..."}. Falls back to a body snippet.
func parseErrorMessage(data []byte) string {
	var parsed struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &parsed); err == nil {
		if parsed.Error != "" {
			return parsed.Error
		}
		if parsed.Message != "" {
			return parsed.Message
		}
	}
	snippet := strings.TrimSpace(string(data))
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}
	if snippet == "" {
		return "request failed"
	}
	return snippet
}

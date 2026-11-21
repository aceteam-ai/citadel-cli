package redisapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/workercontrol"
	"github.com/gorilla/websocket"
)

// WorkerControlOperation never uses generic Publish/Acknowledge or retries an
// uncertain operation through another transport. Every reply is typed/correlated.
func (c *Client) HasDeviceToken() bool { return c != nil && strings.TrimSpace(c.token) != "" }
func (c *Client) IndependentWorkerControlClient() *Client {
	return NewClient(ClientConfig{BaseURL: c.baseURL, Token: c.token, Timeout: c.httpClient.Timeout, DebugFunc: c.debugFunc})
}
func (c *Client) WorkerControlOperation(ctx context.Context, r workercontrol.Request) (workercontrol.Response, error) {
	if !c.HasDeviceToken() {
		return workercontrol.Response{}, workercontrol.ErrDenied
	}
	if r.Validate() != nil {
		return workercontrol.Response{}, workercontrol.ErrInvalid
	}
	if c.wsClient.IsConnected() {
		return c.wsClient.workerControlOperation(ctx, r)
	}
	body, err := json.Marshal(r)
	if err != nil {
		return workercontrol.Response{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/fabric/worker-control/operation", bytes.NewReader(body))
	if err != nil {
		return workercontrol.Response{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cache-Control", "no-store")
	// Redirects must not carry device identity or authority into another route.
	transport := *c.httpClient
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := transport.Do(req)
	if err != nil {
		return workercontrol.Response{}, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 16385))
	if err != nil {
		return workercontrol.Response{}, err
	}
	var s workercontrol.Response
	if workercontrol.Decode(raw, &s, 16384) != nil {
		return s, workercontrol.ErrInvalid
	}
	err = workercontrol.ValidateResponse(r, s)
	if response.StatusCode != http.StatusOK {
		if err == nil {
			return s, workercontrol.ErrInvalid
		}
		return s, err
	}
	if s.Decision != "accepted" {
		return s, workercontrol.ErrInvalid
	}
	return s, err
}

type controlReply struct {
	response workercontrol.Response
	err      error
}
type controlWaiter struct {
	conn  *websocket.Conn
	reply chan controlReply
}

// Kept separate from subscriptions and generic message handlers. The socket
// incarnation is part of the correlation, and each waiter is bounded.
type controlWaiters struct {
	mu      sync.Mutex
	pending map[string]*controlWaiter
}

func (c *WSClient) workerControlOperation(ctx context.Context, r workercontrol.Request) (workercontrol.Response, error) {
	if r.Validate() != nil {
		return workercontrol.Response{}, workercontrol.ErrInvalid
	}
	c.connMu.RLock()
	conn := c.conn
	connected := c.connected
	if conn == nil || !connected {
		c.connMu.RUnlock()
		return workercontrol.Response{}, workercontrol.ErrUnavailable
	}
	waiter := &controlWaiter{conn: conn, reply: make(chan controlReply, 1)}
	c.control.mu.Lock()
	if c.control.pending == nil {
		c.control.pending = map[string]*controlWaiter{}
	}
	if len(c.control.pending) >= 32 || c.control.pending[r.RequestID] != nil {
		c.control.mu.Unlock()
		c.connMu.RUnlock()
		return workercontrol.Response{}, workercontrol.ErrUnavailable
	}
	c.control.pending[r.RequestID] = waiter
	c.control.mu.Unlock()
	c.connMu.RUnlock()
	defer func() {
		c.control.mu.Lock()
		if c.control.pending[r.RequestID] == waiter {
			delete(c.control.pending, r.RequestID)
		}
		c.control.mu.Unlock()
	}()
	frame := struct {
		Type    string                `json:"type"`
		Control workercontrol.Request `json:"control"`
	}{"worker_control_request", r}
	raw, err := json.Marshal(frame)
	if err != nil {
		return workercontrol.Response{}, err
	}
	c.writeMu.Lock()
	if ctx.Err() != nil {
		c.writeMu.Unlock()
		return workercontrol.Response{}, ctx.Err()
	}
	deadline := time.Now().Add(c.writeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetWriteDeadline(deadline)
	err = conn.WriteMessage(websocket.TextMessage, raw)
	c.writeMu.Unlock()
	if err != nil {
		return workercontrol.Response{}, err
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case reply := <-waiter.reply:
		if ctx.Err() != nil {
			return workercontrol.Response{}, ctx.Err()
		}
		select {
		case <-c.done:
			return workercontrol.Response{}, workercontrol.ErrUnavailable
		default:
		}
		c.connMu.RLock()
		current := c.connected && c.conn == conn
		c.connMu.RUnlock()
		if !current {
			return workercontrol.Response{}, workercontrol.ErrUnavailable
		}
		if reply.err != nil {
			return reply.response, reply.err
		}
		return reply.response, workercontrol.ValidateResponse(r, reply.response)
	case <-ctx.Done():
		return workercontrol.Response{}, ctx.Err()
	case <-c.done:
		return workercontrol.Response{}, workercontrol.ErrUnavailable
	case <-timer.C:
		return workercontrol.Response{}, context.DeadlineExceeded
	}
}
func (c *WSClient) failControlWaiters(conn *websocket.Conn) {
	c.control.mu.Lock()
	defer c.control.mu.Unlock()
	for id, w := range c.control.pending {
		if conn == nil || w.conn == conn {
			delete(c.control.pending, id)
			w.reply <- controlReply{err: workercontrol.ErrUnavailable}
		}
	}
}
func (c *WSClient) controlFrame(conn *websocket.Conn, raw []byte) bool {
	// Only an outer response discriminator can affect control waiters. Scan
	// every occurrence before strict decoding rejects duplicate/aliased names.
	if !workercontrol.HasStringField(raw, "worker_control_response", "type") {
		return false
	}
	var frame struct {
		Type    string                 `json:"type"`
		Control workercontrol.Response `json:"control"`
	}
	if workercontrol.Decode(raw, &frame, 20000) != nil {
		c.failControlWaiters(conn)
		return true
	}
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	if !c.connected || c.conn != conn {
		return true
	}
	c.control.mu.Lock()
	defer c.control.mu.Unlock()
	w := c.control.pending[frame.Control.RequestID]
	if w != nil && w.conn == conn {
		delete(c.control.pending, frame.Control.RequestID)
		w.reply <- controlReply{response: frame.Control}
	}
	return true
}

// Strict control delivery parsing runs while duplicate outer names still exist.
func validateControlWSJob(raw []byte) error {
	var frame struct {
		Type  string             `json:"type"`
		Queue string             `json:"queue"`
		ID    string             `json:"id"`
		Data  workercontrol.Data `json:"data"`
	}
	if workercontrol.Decode(raw, &frame, 20000) != nil || frame.Type != "job" {
		return workercontrol.ErrInvalid
	}
	d, _ := json.Marshal(frame.Data)
	_, _, err := workercontrol.ParseData(d, frame.ID, frame.Queue, workercontrol.Protocol)
	return err
}

func (c *WorkerControlConfig) UnmarshalJSON(raw []byte) error {
	// Legacy metadata remains readable for normal configuration but never
	// opts in. Only v2 metadata takes the strict executable schema path.
	var version struct {
		Protocol string `json:"protocol"`
	}
	if json.Unmarshal(raw, &version) != nil {
		return workercontrol.ErrInvalid
	}
	if version.Protocol != "worker-control-v2" {
		*c = WorkerControlConfig{Protocol: version.Protocol}
		return nil
	}
	var config workercontrol.Config
	if workercontrol.Decode(raw, &config, 8192) != nil || config.Validate() != nil {
		return workercontrol.ErrInvalid
	}
	*c = WorkerControlConfig(config)
	return nil
}
func (m *StreamMessage) UnmarshalJSON(raw []byte) error {
	type plain StreamMessage
	if !workercontrol.LooksControlData(raw, "data") {
		return json.Unmarshal(raw, (*plain)(m))
	}
	var strict struct {
		ID   string             `json:"id"`
		Data workercontrol.Data `json:"data"`
	}
	if workercontrol.Decode(raw, &strict, 20000) != nil {
		return workercontrol.ErrInvalid
	}
	data, _ := json.Marshal(strict.Data)
	if _, _, err := workercontrol.ParseData(data, strict.ID, "", ""); err != nil {
		return err
	}
	*m = StreamMessage{ID: strict.ID, Data: StreamMessageData{JobID: strict.Data.JobID, Type: strict.Data.Type, Payload: strict.Data.Payload, WorkerControl: strict.Data.WorkerControl}}
	return nil
}

func (r *ConsumeResponse) UnmarshalJSON(raw []byte) error {
	type plain ConsumeResponse
	if workercontrol.LooksControlData(raw, "messages", "*", "data") && workercontrol.CheckJSON(raw, 4<<20) != nil {
		return workercontrol.ErrInvalid
	}
	return json.Unmarshal(raw, (*plain)(r))
}
func (r *WorkerConfigResponse) UnmarshalJSON(raw []byte) error {
	type plain WorkerConfigResponse
	if workercontrol.HasStringField(raw, workercontrol.Protocol, "worker_control", "protocol") && workercontrol.CheckJSON(raw, 65536) != nil {
		return workercontrol.ErrInvalid
	}
	return json.Unmarshal(raw, (*plain)(r))
}

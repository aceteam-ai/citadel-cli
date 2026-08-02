// internal/jobs/huddle_realtime_conn.go
//
// Production realtimeConn: a gorilla/websocket client to the AceTeam realtime
// engine (wss://<api>/api/realtime), used by the HUDDLE_JOIN converse bridge.
//
// Auth: the AceTeam WS server (aceteam middleware/websocketServer.ts) accepts an
// `Authorization: Bearer <token>` HEADER from device clients (citadel-cli) and
// authenticates it via the same `authenticate()` path as the REST API -- i.e. an
// `act_` API key or a Supabase user JWT. The huddle-bot mint token is NOT known to
// be accepted there, so the bridge takes an EXPLICIT realtime token (payload
// `realtime_token` / env CITADEL_REALTIME_TOKEN) rather than reusing the mint
// token. See huddle_join.go's resolveRealtimeToken.
//
// Turn detection: `turnDetection=server_vad` rides the URL query so the engine
// runs server-side VAD (the bridge never forces a turn with commit/response.create).
package jobs

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// realtimeDialTimeout bounds the WS handshake (not the session).
const realtimeDialTimeout = 20 * time.Second

// gorillaRealtimeConn adapts a *websocket.Conn to the realtimeConn seam. gorilla
// allows a single READER and a single WRITER at a time. recvLoop is the only
// reader, but TWO goroutines call Send (hearLoop's append and speakerLoop's
// clear), so writes MUST be serialized here — writeMu does that (gorilla panics
// with "concurrent write to websocket connection" otherwise).
type gorillaRealtimeConn struct {
	c       *websocket.Conn
	writeMu sync.Mutex
}

func (g *gorillaRealtimeConn) Send(payload []byte) error {
	g.writeMu.Lock()
	defer g.writeMu.Unlock()
	return g.c.WriteMessage(websocket.TextMessage, payload)
}

func (g *gorillaRealtimeConn) Recv() ([]byte, error) {
	_, data, err := g.c.ReadMessage()
	return data, err
}

func (g *gorillaRealtimeConn) Close() error {
	return g.c.Close()
}

// dialRealtime builds the realtime WS URL from the API base (or an explicit
// override) and dials it with the Bearer token. agentID is passed as a query param
// so the engine resolves the right agent session; turnDetection=server_vad opts
// into hands-free turn taking.
func dialRealtime(apiBase, explicitURL, token, agentID string) (realtimeConn, error) {
	wsURL, err := realtimeWSURL(apiBase, explicitURL, agentID)
	if err != nil {
		return nil, err
	}
	dialer := &websocket.Dialer{HandshakeTimeout: realtimeDialTimeout}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)
	c, resp, err := dialer.Dial(wsURL, hdr)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("dial realtime ws (status %d): %w", resp.StatusCode, err)
		}
		return nil, fmt.Errorf("dial realtime ws: %w", err)
	}
	return &gorillaRealtimeConn{c: c}, nil
}

// realtimeWSURL derives the wss:// realtime endpoint. An explicit realtime_url
// wins (its query is preserved and agentId/turnDetection are ensured); otherwise
// it is {api as ws}/api/realtime?agentId=..&turnDetection=server_vad.
func realtimeWSURL(apiBase, explicitURL, agentID string) (string, error) {
	raw := explicitURL
	if raw == "" {
		raw = strings.TrimRight(apiBase, "/") + "/api/realtime"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse realtime url %q: %w", raw, err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "wss", "ws":
		// already a ws scheme
	default:
		return "", fmt.Errorf("unsupported realtime url scheme %q", u.Scheme)
	}
	q := u.Query()
	if agentID != "" && q.Get("agentId") == "" {
		q.Set("agentId", agentID)
	}
	if q.Get("turnDetection") == "" {
		q.Set("turnDetection", "server_vad")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

var _ realtimeConn = (*gorillaRealtimeConn)(nil)

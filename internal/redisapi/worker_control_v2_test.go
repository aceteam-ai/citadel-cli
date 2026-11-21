package redisapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	wc "github.com/aceteam-ai/citadel-cli/internal/workercontrol"
	"github.com/aceteam-ai/citadel-cli/internal/workercontrol/controltest"
	"github.com/gorilla/websocket"
)

func TestWorkerControlV2RESTRequiresTypedCorrelatedDecision(t *testing.T) {
	for _, mode := range []string{"accepted", "denied", "unavailable", "empty", "generic", "wrong_attempt", "duplicate", "fraction", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			f := controltest.Load()
			request := f.Request("claimA")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/fabric/worker-control/operation" || r.Header.Get("Authorization") != "Bearer synthetic-token" || r.Header.Get("Cache-Control") != "no-store" {
					t.Error("wrong operation/auth/cache boundary")
					w.WriteHeader(403)
					return
				}
				var got wc.Request
				if json.NewDecoder(r.Body).Decode(&got) != nil || got != request {
					t.Error("request changed")
				}
				response := controltest.Accept(got, 1800000001001)
				switch mode {
				case "denied":
					response.Decision = "denied"
					response.Code = "expired"
					response.Receipt = nil
					response.Observation = nil
					w.WriteHeader(409)
				case "unavailable":
					response.Decision = "unavailable"
					response.Code = "authority_unavailable"
					response.Receipt = nil
					response.Observation = nil
					w.WriteHeader(503)
				case "empty":
					return
				case "generic":
					_, _ = w.Write([]byte(`{"published":true}`))
					return
				case "wrong_attempt":
					response.Identity.AttemptID = response.Identity.JobID
				case "redirect":
					w.Header().Set("Location", "/generic")
					w.WriteHeader(307)
					return
				}
				raw, _ := json.Marshal(response)
				if mode == "duplicate" {
					raw = []byte(strings.Replace(string(raw), `"state_version":2`, `"state_version":2,"state_version":2`, 1))
				}
				if mode == "fraction" {
					raw = []byte(strings.Replace(string(raw), `"binding_version":7`, `"binding_version":7.0`, 1))
				}
				_, _ = w.Write(raw)
			}))
			defer srv.Close()
			c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "synthetic-token"})
			defer c.Close()
			_, err := c.WorkerControlOperation(context.Background(), request)
			if mode == "accepted" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("transport success was mistaken for authority")
			}
			if mode == "denied" && !errors.Is(err, wc.ErrDenied) {
				t.Fatal("typed denial lost", err)
			}
			if mode == "unavailable" && !errors.Is(err, wc.ErrUnavailable) {
				t.Fatal("typed unavailability lost", err)
			}
		})
	}
}

func TestWorkerControlV2WebSocketDecisionAndNoGenericFallback(t *testing.T) {
	for _, mode := range []string{"accepted", "denied", "silent", "generic_error", "wrong_attempt", "wrong_id_then_right", "duplicate", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			var genericCalls atomic.Int32
			upgrader := websocket.Upgrader{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/fabric/redis/ws" {
					genericCalls.Add(1)
					w.WriteHeader(403)
					return
				}
				if r.Header.Get("Authorization") != "Bearer synthetic-token" {
					t.Error("unauthenticated socket")
					w.WriteHeader(401)
					return
				}
				conn, e := upgrader.Upgrade(w, r, nil)
				if e != nil {
					return
				}
				defer conn.Close()
				var frame struct {
					Type    string     `json:"type"`
					Control wc.Request `json:"control"`
				}
				if conn.ReadJSON(&frame) != nil {
					return
				}
				if frame.Type != "worker_control_request" || frame.Control.Validate() != nil {
					t.Error("generic control publication")
					return
				}
				response := controltest.Accept(frame.Control, 1800000001001)
				switch mode {
				case "denied":
					response.Decision = "denied"
					response.Code = "expired"
					response.Receipt = nil
					response.Observation = nil
				case "silent":
					var discard any
					_ = conn.ReadJSON(&discard)
					return
				case "generic_error":
					_ = conn.WriteJSON(map[string]any{"type": "error", "error": "Failed to publish message"})
					var discard any
					_ = conn.ReadJSON(&discard)
					return
				case "wrong_attempt":
					response.Identity.AttemptID = response.Identity.JobID
				case "wrong_id_then_right":
					wrong := response
					wrong.RequestID = controltest.Load().Cases["claimB"].RequestID
					_ = conn.WriteJSON(map[string]any{"type": "worker_control_response", "control": wrong})
				case "disconnect":
					return
				}
				raw, _ := json.Marshal(map[string]any{"type": "worker_control_response", "control": response})
				if mode == "duplicate" {
					raw = []byte(strings.Replace(string(raw), `"state_version":2`, `"state_version":2,"state_version":2`, 1))
				}
				_ = conn.WriteMessage(websocket.TextMessage, raw)
				var discard any
				_ = conn.ReadJSON(&discard)
			}))
			defer srv.Close()
			c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "synthetic-token"})
			defer c.Close()
			if e := c.EnableWebSocket(context.Background()); e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, err := c.WorkerControlOperation(ctx, controltest.Load().Request("claimA"))
			if mode == "accepted" || mode == "wrong_id_then_right" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("socket write authorized a control action")
			}
			if genericCalls.Load() != 0 {
				t.Fatal("uncertain WS operation fell back to generic HTTP")
			}
		})
	}
}

func TestWorkerControlV2WaitersAreConnectionScopedAndBounded(t *testing.T) {
	c := NewWSClient(WSClientConfig{})
	a, b := &websocket.Conn{}, &websocket.Conn{}
	c.conn = a
	c.connected = true
	f := controltest.Load()
	r := f.Request("claimA")
	s := controltest.Accept(r, 1800000001001)
	raw, _ := json.Marshal(map[string]any{"type": "worker_control_response", "control": s})
	w := &controlWaiter{conn: a, reply: make(chan controlReply, 1)}
	c.control.pending = map[string]*controlWaiter{r.RequestID: w}
	c.controlFrame(b, raw)
	select {
	case <-w.reply:
		t.Fatal("prior socket response resolved current waiter")
	default:
	}
	c.controlFrame(a, raw)
	select {
	case v := <-w.reply:
		if v.response.RequestID != r.RequestID {
			t.Fatal("wrong correlation")
		}
	default:
		t.Fatal("exact reply lost")
	}
	c.controlFrame(a, raw) // duplicates cannot fill a completed waiter.
	w = &controlWaiter{conn: a, reply: make(chan controlReply, 1)}
	c.control.pending[r.RequestID] = w
	c.failControlWaiters(b)
	if len(c.control.pending) != 1 {
		t.Fatal("unrelated disconnect cancelled waiter")
	}
	c.failControlWaiters(a)
	if len(c.control.pending) != 0 {
		t.Fatal("disconnected waiter retained")
	}
	for n := 0; n < 32; n++ {
		c.control.pending[fmt.Sprint(n)] = &controlWaiter{conn: a, reply: make(chan controlReply, 1)}
	}
	if _, err := c.workerControlOperation(context.Background(), r); !errors.Is(err, wc.ErrUnavailable) {
		t.Fatal("unbounded waiter admission")
	}
}

func TestWorkerControlV2ReorderedResponsesResolveOnlyTheirWaiter(t *testing.T) {
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, e := upgrader.Upgrade(w, r, nil)
		if e != nil {
			return
		}
		defer conn.Close()
		var frames [2]struct {
			Type    string     `json:"type"`
			Control wc.Request `json:"control"`
		}
		for n := range frames {
			if conn.ReadJSON(&frames[n]) != nil {
				return
			}
		}
		// This models transport ordering only, without granting executable actions.
		for n := 1; n >= 0; n-- {
			_ = conn.WriteJSON(map[string]any{"type": "worker_control_response", "control": controltest.Accept(frames[n].Control, 1800000001001)})
		}
		var discard any
		_ = conn.ReadJSON(&discard)
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "synthetic-token"})
	defer c.Close()
	if c.EnableWebSocket(context.Background()) != nil {
		t.Fatal("connect")
	}
	results := make(chan error, 2)
	f := controltest.Load()
	for _, name := range []string{"claimA", "claimB"} {
		r := f.Request(name)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			s, e := c.WorkerControlOperation(ctx, r)
			if e == nil && s.RequestID != r.RequestID {
				e = wc.ErrInvalid
			}
			results <- e
		}()
	}
	for n := 0; n < 2; n++ {
		if e := <-results; e != nil {
			t.Fatal(e)
		}
	}
}

func TestWorkerControlV2RawDeliveryAdapters(t *testing.T) {
	f := controltest.Load()
	data := f.Data()
	frame := map[string]any{"type": "job", "queue": f.Identity.Queue, "id": f.Deliveries["A"].MessageID, "data": data}
	raw, _ := json.Marshal(frame)
	if validateControlWSJob(raw) != nil {
		t.Fatal("valid WS control framing rejected")
	}
	for name, bad := range map[string]string{
		"duplicate outer": strings.Replace(string(raw), `"type":"job"`, `"type":"job","type":"job"`, 1),
		"extra outer":     strings.TrimSuffix(string(raw), "}") + `,"unknown":true}`,
		"wrong queue":     strings.Replace(string(raw), `"queue":"jobs:v2:control:`, `"queue":"jobs:v1:control:`, 1),
		"nested control":  strings.Replace(string(raw), `"worker_control":"`, `"worker_control":{ "injected":"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if validateControlWSJob([]byte(bad)) == nil {
				t.Fatal("ambiguous WS control accepted")
			}
		})
	}
	body, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"id": f.Deliveries["A"].MessageID, "data": data}}})
	var response ConsumeResponse
	if json.Unmarshal(body, &response) != nil {
		t.Fatal("valid REST control rejected")
	}
	job, e := ParseStreamMessage(response.Messages[0])
	if e != nil || job.Control == nil {
		t.Fatal("REST dropped typed control")
	}
	duplicate := strings.Replace(string(body), `"type":"WORKER_CONTROL"`, `"type":"WORKER_CONTROL","type":"SHELL_COMMAND"`, 1)
	if json.Unmarshal([]byte(duplicate), &response) == nil {
		t.Fatal("duplicate control classification downgraded to generic")
	}
}

package redisapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	wc "github.com/aceteam-ai/citadel-cli/internal/workercontrol"
	"github.com/aceteam-ai/citadel-cli/internal/workercontrol/controltest"
	"github.com/gorilla/websocket"
)

func ordinaryControlMarkers() []string {
	return []string{"worker_control", "WORKER_CONTROL", "worker_control_response", "worker_control_request", controltest.Load().Identity.Queue, "jobs:v1:control:node-test"}
}

func ordinaryMarkerData(marker string) map[string]string {
	payload, _ := json.Marshal(map[string]any{"content": marker, marker: map[string]any{"type": marker, "worker_control": marker}})
	return map[string]string{"jobId": "ordinary-job", "type": "SHELL_COMMAND", "payload": string(payload), "rayId": marker, "enqueuedAt": marker}
}

func TestWorkerControlOrdinaryMarkersActualReadLoop(t *testing.T) {
	markers := ordinaryControlMarkers()
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var request struct {
			Type    string     `json:"type"`
			Control wc.Request `json:"control"`
		}
		if conn.ReadJSON(&request) != nil {
			return
		}
		// An overwritten transport discriminator must still be refused before
		// normal and wildcard handlers, even when the final job looks ordinary.
		badJob := fmt.Sprintf(`{"type":"job","queue":%q,"queue":"jobs:v1:cpu-general","id":"1-0","data":{"type":"SHELL_COMMAND","payload":"{}"}}`, controltest.Load().Identity.Queue)
		_ = conn.WriteMessage(websocket.TextMessage, []byte(badJob))
		for _, marker := range markers {
			// The real pubsub envelope carries arbitrary application content.
			_ = conn.WriteJSON(map[string]any{"type": "message", "channel": "stream:v1:ordinary", "message": map[string]any{"type": "chunk", "data": map[string]any{"content": marker, marker: marker, "type": marker, "worker_control": marker}}})
			_ = conn.WriteJSON(map[string]any{"type": "job", "queue": "jobs:v1:cpu-general", "id": "1-0", "data": ordinaryMarkerData(marker)})
		}
		_ = conn.WriteJSON(map[string]any{"type": "worker_control_response", "control": controltest.Accept(request.Control, 1800000001001)})
		var discard any
		_ = conn.ReadJSON(&discard)
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "synthetic-token"})
	defer c.Close()
	messages := make(chan WSMessage, len(markers))
	jobs := make(chan WSMessage, len(markers))
	wildcard := make(chan WSMessage, 2*len(markers)+1)
	c.wsClient.OnMessage("message", func(m WSMessage) { messages <- m })
	c.wsClient.OnMessage("job", func(m WSMessage) { jobs <- m })
	c.wsClient.OnMessage("*", func(m WSMessage) { wildcard <- m })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.EnableWebSocket(ctx); err != nil {
		t.Fatal(err)
	}
	request := controltest.Load().Request("claimA")
	response, err := c.WorkerControlOperation(ctx, request)
	if err != nil || response.RequestID != request.RequestID {
		t.Fatal("ordinary content cancelled or resolved the pending control request", err)
	}
	for _, marker := range markers {
		select {
		case m := <-messages:
			if m.Channel != "stream:v1:ordinary" || m.Message["data"].(map[string]any)["content"] != marker {
				t.Fatal("ordinary pubsub content changed")
			}
		case <-ctx.Done():
			t.Fatal("ordinary pubsub message was dropped")
		}
		select {
		case m := <-jobs:
			if m.Data["rayId"] != marker || m.Data["payload"] != ordinaryMarkerData(marker)["payload"] {
				t.Fatal("ordinary job changed")
			}
		case <-ctx.Done():
			t.Fatal("ordinary job was dropped")
		}
		for _, typ := range []string{"message", "job"} {
			select {
			case m := <-wildcard:
				if m.Type != typ {
					t.Fatal("wildcard delivery changed")
				}
			case <-ctx.Done():
				t.Fatal("ordinary wildcard message was dropped")
			}
		}
	}
	select {
	case <-wildcard:
		t.Fatal("control response reached an ordinary handler")
	default:
	}
}

func TestWorkerControlOrdinaryMarkersActualConsume(t *testing.T) {
	for _, marker := range ordinaryControlMarkers() {
		t.Run(marker, func(t *testing.T) {
			data := ordinaryMarkerData(marker)
			body, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"id": "1-0", "data": data}}, "ordinary": marker})
			// Ordinary decoding permits unused fields and non-integral numbers.
			body = []byte(strings.TrimSuffix(string(body), "}") + `,"unused":1.5,"unused":2.5}`)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/fabric/redis/jobs/consume" {
					t.Error("wrong consume route")
				}
				_, _ = w.Write(body)
			}))
			defer srv.Close()
			c := NewClient(ClientConfig{BaseURL: srv.URL})
			defer c.Close()
			job, err := c.ConsumeJob(context.Background(), ConsumeRequest{Queue: "jobs:v1:cpu-general", Group: "citadel-workers", Consumer: "test", Count: 1, BlockMs: 1})
			if err != nil || job == nil {
				t.Fatal("ordinary consume rejected marker content", err)
			}
			if job.Control != nil || job.Type != "SHELL_COMMAND" || job.Payload["content"] != marker || string(job.RawPayload) != data["payload"] || job.RawData["rayId"] != marker {
				t.Fatal("ordinary consume content or type changed")
			}
		})
	}
}

func TestWorkerControlDiscriminatorsCannotBeOverwritten(t *testing.T) {
	f := controltest.Load()
	data, _ := json.Marshal(f.Data())
	job := fmt.Sprintf(`{"type":"job","queue":%q,"id":%q,"data":%s}`, f.Identity.Queue, f.Deliveries["A"].MessageID, data)
	response, _ := json.Marshal(map[string]any{"type": "worker_control_response", "control": controltest.Accept(f.Request("claimA"), 1800000001001)})
	for name, raw := range map[string]string{
		"duplicate type":      strings.Replace(job, `"type":"job"`, `"type":"job","type":"message"`, 1),
		"duplicate queue":     strings.Replace(job, `"queue":`, `"queue":"jobs:v1:cpu-general","queue":`, 1),
		"duplicate data":      strings.TrimSuffix(job, "}") + `,"data":{"type":"SHELL_COMMAND","payload":"{}"}}`,
		"duplicate data type": strings.Replace(job, `"type":"WORKER_CONTROL"`, `"type":"WORKER_CONTROL","type":"SHELL_COMMAND"`, 1),
		"aliased data type":   strings.Replace(job, `"type":"WORKER_CONTROL"`, `"TYPE":"WORKER_CONTROL","type":"SHELL_COMMAND"`, 1),
		"escaped duplicate":   strings.Replace(job, `"type":"WORKER_CONTROL"`, `"t\u0079pe":"WORKER_CONTROL","type":"SHELL_COMMAND"`, 1),
		"bad unicode":         strings.Replace(job, `"type":"job"`, `"type":"job","extra":"\ud800"`, 1),
		"malformed data":      `{"type":"job","data":{"worker_control":`,
	} {
		t.Run(name, func(t *testing.T) {
			if !wc.LooksControl([]byte(raw)) || validateControlWSJob([]byte(raw)) == nil {
				t.Fatal("invalid control job escaped strict classification")
			}
		})
	}
	for name, raw := range map[string]string{
		"duplicate response": strings.Replace(string(response), `"type":"worker_control_response"`, `"type":"worker_control_response","type":"message"`, 1),
		"aliased response":   strings.Replace(string(response), `"type":"worker_control_response"`, `"TYPE":"worker_control_response","type":"message"`, 1),
		"escaped duplicate":  strings.Replace(string(response), `"type":"worker_control_response"`, `"t\u0079pe":"worker_control_response","type":"message"`, 1),
		"malformed response": `{"type":"worker_control_response","control":`,
	} {
		t.Run(name, func(t *testing.T) {
			c := NewWSClient(WSClientConfig{})
			conn, other := &websocket.Conn{}, &websocket.Conn{}
			waiter := &controlWaiter{conn: conn, reply: make(chan controlReply, 1)}
			unrelated := &controlWaiter{conn: other, reply: make(chan controlReply, 1)}
			c.control.pending = map[string]*controlWaiter{"current": waiter, "other": unrelated}
			if !c.controlFrame(conn, []byte(raw)) {
				t.Fatal("malformed control reply reached generic routing")
			}
			select {
			case reply := <-waiter.reply:
				if reply.err == nil {
					t.Fatal("malformed response completed waiter successfully")
				}
			default:
				t.Fatal("malformed response did not fail closed")
			}
			select {
			case <-unrelated.reply:
				t.Fatal("malformed response cancelled another socket's waiter")
			default:
			}
		})
	}
}

func TestWorkerControlConsumeDuplicateDiscriminatorsRejected(t *testing.T) {
	f := controltest.Load()
	data, _ := json.Marshal(f.Data())
	message := fmt.Sprintf(`{"id":%q,"data":%s}`, f.Deliveries["A"].MessageID, data)
	for name, body := range map[string]string{
		"overwritten messages": `{"messages":[` + message + `],"messages":[]}`,
		"aliased messages":     `{"MESSAGES":[` + message + `],"messages":[]}`,
		"escaped messages":     `{"me\u0073sages":[` + message + `],"messages":[]}`,
		"unicode alias":        `{"me\u017f\u017fage\u017f":[` + message + `],"messages":[]}`,
		"overwritten data":     `{"messages":[` + strings.TrimSuffix(message, "}") + `,"data":{"type":"SHELL_COMMAND","payload":"{}"}}]}`,
		"aliased data":         `{"messages":[` + strings.Replace(message, `"data":`, `"DATA":`, 1) + `]}`,
		"overwritten type":     `{"messages":[` + strings.Replace(message, `"type":"WORKER_CONTROL"`, `"type":"WORKER_CONTROL","type":"SHELL_COMMAND"`, 1) + `]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var got ConsumeResponse
			if json.Unmarshal([]byte(body), &got) == nil {
				t.Fatal("control consume discriminator escaped strict decoding")
			}
		})
	}
}

package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/deliverymetadata"
	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	"github.com/gorilla/websocket"
)

const deliveryTestID = "b386d2ec-f65d-4791-a9de-f5b78b09dce3"

var deliveryClaim = base64.RawURLEncoding.EncodeToString([]byte("bounded-claim-test-only-32-bytes!!")[:32])

func deliveryData() string {
	return fmt.Sprintf(`{"jobId":%q,"type":"SHELL_COMMAND","payload":"{ \"n\":9007199254740993,\"text\":\"雪\" }","protocol":"durable-results-v1","dispatchId":%q,"requestDigest":%q,"claimToken":%q,"leaseEpoch":"9007199254740991"}`, deliveryTestID, deliveryTestID, strings.Repeat("a", 64), deliveryClaim)
}

func assertDeliveryJob(t *testing.T, job *Job, queue, transport string) {
	t.Helper()
	if job == nil || job.observation.State() != deliverymetadata.Observed {
		t.Fatal("actual transport observation not retained")
	}
	want := deliverymetadata.Identity{JobID: deliveryTestID, Type: "SHELL_COMMAND", MessageID: "1-0", Queue: queue, RequestedGroup: "citadel-workers", Transport: transport}
	if job.observation.Identity() != want {
		t.Fatal("original/local identity mismatch")
	}
	for _, name := range []string{"protocol", "dispatchId", "requestDigest", "claimToken", "leaseEpoch"} {
		if job.observation.Presence(name) != deliverymetadata.Captured {
			t.Fatal("field dropped")
		}
	}
	if string(job.RawPayload) != `{ "n":9007199254740993,"text":"雪" }` {
		t.Fatal("raw payload changed")
	}
	job.ID = "mutated"
	job.Type = "mutated"
	job.MessageID = "mutated"
	job.SourceQueue = "mutated"
	job.Payload["n"] = 1
	if job.observation.Identity() != want {
		t.Fatal("wire identity rebound")
	}
	with, _ := json.Marshal(job)
	copy := *job
	copy.observation = nil
	without, _ := json.Marshal(&copy)
	if string(with) != string(without) {
		t.Fatal("public marshal changed")
	}
	for _, sentinel := range []string{deliveryClaim, base64.StdEncoding.EncodeToString([]byte(deliveryClaim)), `"observation"`, `"claimToken"`, `"leaseEpoch"`} {
		if strings.Contains(string(with), sentinel) {
			t.Fatal("private metadata marshaled")
		}
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%d", "%f", "%q", "%p", "%T", "%z", "%#020.12f"} {
		for _, parent := range []any{job, *job, []*Job{job}, map[string]*Job{"entry": job}} {
			out := fmt.Sprintf(format, parent)
			for _, sentinel := range []string{deliveryClaim, base64.StdEncoding.EncodeToString([]byte(deliveryClaim)), deliveryTestID} {
				if strings.Contains(out, sentinel) {
					t.Fatalf("private format %s leaked", format)
				}
			}
		}
	}
}

func TestDeliveryObservationAPIActualConsumeConversion(t *testing.T) {
	var req map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/fabric/redis/jobs/consume" {
			t.Errorf("unexpected service call")
		}
		json.NewDecoder(r.Body).Decode(&req)
		fmt.Fprint(w, `{"messages":[{"id":"1-0","data":`+deliveryData()+`}]}`)
	}))
	defer srv.Close()
	source := NewAPISource(APISourceConfig{BaseURL: srv.URL, Token: "test-only", QueueNames: []string{"requested"}})
	source.client = redisapi.NewClient(redisapi.ClientConfig{BaseURL: srv.URL, Token: "test-only"})
	defer source.Close()
	job, err := source.nextSingle(context.Background(), "actual-request-queue", 1)
	// S3a refuses legacy-negotiated v1 metadata; a passive observation is not
	// an authenticated, renewed execution claim.
	if err == nil || job != nil {
		t.Fatal("passive API metadata acquired execution authority")
	}
	if req["queue"] != "actual-request-queue" || req["group"] != "citadel-workers" || req["consumer"] != source.client.WorkerID() || req["protocol"] != nil || len(req) != 5 {
		t.Fatal("request negotiation changed")
	}
	source.queueNames[0] = "changed"
	apiJob, err := source.client.ConsumeJob(context.Background(), redisapi.ConsumeRequest{Queue: "original", Group: "citadel-workers", Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !apiJob.HasDurableMetadata() || apiJob.ClaimedDelivery() != nil {
		t.Fatal("passive consume minted or dropped claim provenance")
	}
	assertDeliveryJob(t, source.convertJob(apiJob), "original", "redis-api")
	apiJob.JobID = "replacement"
	apiJob.Type = "replacement"
	apiJob.MessageID = "replacement"
	one, two := source.convertJob(apiJob), source.convertJob(apiJob)
	if one.observation == two.observation || one.observation.Identity().JobID != deliveryTestID || one.observation.Identity().MessageID != "1-0" {
		t.Fatal("conversion aliases/rebinds observation")
	}
	one.RawPayload[0] = 'x'
	if two.RawPayload[0] == 'x' || apiJob.RawPayload[0] == 'x' {
		t.Fatal("raw ownership changed")
	}
}

func TestDeliveryObservationWSActualConsumeConversion(t *testing.T) {
	upgrader := websocket.Upgrader{}
	requests := make(chan map[string]any, 1)
	refused := make(chan struct{}, 1)
	finish := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			if r.URL.Path != "/api/fabric/redis/ping" {
				t.Errorf("unexpected HTTP service call")
			}
			fmt.Fprint(w, `{}`)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var req map[string]any
		json.Unmarshal(raw, &req)
		requests <- req
		conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"terminal_duplicate","data":{}}`))
		conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"job","queue":"wire-queue","id":"1-0","data":`+deliveryData()+`}`))
		<-finish
	}))
	defer srv.Close()
	client := redisapi.NewClient(redisapi.ClientConfig{BaseURL: srv.URL, Token: "test-only"})
	defer client.Close()
	source := NewWSSource(WSSourceConfig{Client: client, QueueName: "requested-queue", LogFn: func(string, string) {}, DebugFunc: func(format string, _ ...any) {
		if strings.Contains(format, "failed to convert job message") {
			select {
			case refused <- struct{}{}:
			default:
			}
		}
	}})
	defer source.Close()
	defer close(finish)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := source.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-refused:
	case <-ctx.Done():
		t.Fatal("v1 WebSocket delivery was not refused")
	}
	select {
	case req := <-requests:
		if req["type"] != "consume" || req["group"] != "citadel-workers" || req["consumer"] != client.WorkerID() || req["protocol"] != nil {
			t.Fatal("WS consume negotiation changed")
		}
	case <-ctx.Done():
		t.Fatal("consume not sent")
	}
	source.queueNames[0] = "changed"
	select {
	case <-source.jobs:
		t.Fatal("disposition acquired job authority")
	default:
	}
}

func TestDeliveryObservationUnknownManualAndStaticContract(t *testing.T) {
	api := (&APISource{}).convertJob(&redisapi.Job{JobID: "legacy"})
	if api.observation.State() != deliverymetadata.Unknown {
		t.Fatal("manual API provenance invented")
	}
	var data map[string]string
	json.Unmarshal([]byte(deliveryData()), &data)
	source := NewWSSource(WSSourceConfig{})
	job, err := source.convertWSJob(redisapi.WSMessage{Type: "job", ID: "1-0", Queue: "q", Data: data})
	if err == nil || job != nil {
		t.Fatal("manual v1 WS map acquired execution authority")
	}
	job, err = source.convertWSJob(redisapi.WSMessage{Type: "job", ID: "1-0", Queue: "q", Data: map[string]string{"jobId": "legacy", "payload": "{}"}})
	if err != nil || job.observation.State() != deliverymetadata.Unknown {
		t.Fatal("manual legacy WS provenance invented", err)
	}
	field, ok := reflect.TypeFor[Job]().FieldByName("observation")
	if !ok || field.PkgPath == "" || field.Type.Kind() != reflect.Pointer || field.Tag.Get("json") != "-" {
		t.Fatal("private pointer/JSON tag contract changed")
	}
}

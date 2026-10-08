package redisapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/deliverymetadata"
	"github.com/gorilla/websocket"
)

const observationJobID = "b386d2ec-f65d-4791-a9de-f5b78b09dce3"

var observationClaim = base64.RawURLEncoding.EncodeToString([]byte("bounded-claim-test-only-32-bytes!!")[:32])

func observationData() string {
	return fmt.Sprintf(`{"jobId":%q,"type":"SHELL_COMMAND","payload":"{ \"n\":9007199254740993,\"text\":\"雪\" }","protocol":"durable-results-v1","dispatchId":%q,"requestDigest":%q,"claimToken":%q,"leaseEpoch":"9007199254740991"}`, observationJobID, observationJobID, strings.Repeat("a", 64), observationClaim)
}
func observationResponse(data string) string {
	return `{"messages":[{"id":"1-0","data":` + data + `}]}`
}

func TestDeliveryObservationHTTPConsumeBoundary(t *testing.T) {
	var request map[string]any
	var debug []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/fabric/redis/jobs/consume" {
			t.Errorf("unexpected route")
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, observationResponse(observationData()))
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "test-only", DebugFunc: func(format string, args ...any) { debug = append(debug, fmt.Sprintf(format, args...)) }})
	defer c.Close()
	job, err := c.ConsumeJob(context.Background(), ConsumeRequest{Queue: "requested", Group: "citadel-workers", Consumer: "local-consumer", Count: 1, BlockMs: 1})
	if err != nil {
		t.Fatal(err)
	}
	o := job.DeliveryObservation()
	if o.State() != deliverymetadata.Observed {
		t.Fatalf("state %s", o.State())
	}
	want := deliverymetadata.Identity{JobID: observationJobID, Type: "SHELL_COMMAND", MessageID: "1-0", Queue: "requested", RequestedGroup: "citadel-workers", Transport: "redis-api"}
	if o.Identity() != want {
		t.Fatalf("identity mismatch")
	}
	for _, name := range []string{"protocol", "dispatchId", "requestDigest", "claimToken", "leaseEpoch"} {
		if o.Presence(name) != deliverymetadata.Captured {
			t.Fatal("field dropped")
		}
	}
	if len(request) != 5 || request["queue"] != "requested" || request["group"] != "citadel-workers" || request["consumer"] != "local-consumer" || request["protocol"] != nil {
		t.Fatal("outbound request changed")
	}
	if string(job.RawPayload) != `{ "n":9007199254740993,"text":"雪" }` {
		t.Fatal("exact raw bytes changed")
	}
	if !strings.Contains(strings.Join(debug, "\n"), observationClaim) {
		t.Fatal("baseline raw debug exposure was silently changed")
	}
	job.JobID = "mutated"
	job.Type = "mutated"
	job.MessageID = "mutated"
	job.Payload["n"] = 1
	if o.Identity() != want || job.DeliveryObservation().Identity() != want {
		t.Fatal("original wire identity rebound")
	}
	if job.DeliveryObservation() == job.observation {
		t.Fatal("getter aliases wrapper")
	}
	checkObservationMarshal(t, job, &Job{MessageID: job.MessageID, JobID: job.JobID, Type: job.Type, Payload: job.Payload, RawPayload: job.RawPayload, RawData: job.RawData})
}

func checkObservationMarshal(t *testing.T, captured, baseline any) {
	t.Helper()
	a, err := json.Marshal(captured)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("public serialization changed")
	}
	for _, v := range []string{observationClaim, base64.StdEncoding.EncodeToString([]byte(observationClaim)), `"observation"`, `"claimToken"`, `"leaseEpoch"`} {
		if strings.Contains(string(a), v) {
			t.Fatal("new private metadata marshaled")
		}
	}
}

func TestDeliveryObservationHTTPDecoderCompatibility(t *testing.T) {
	a := `[{"id":"1-0","data":` + observationData() + `}]`
	b := `[{"id":"2-0","data":{"jobId":"other","type":"OTHER","payload":"{}"}}]`
	fixtures := []string{
		`{}`, `null`, `{"messages":null}`, `{"Messages":` + a + `}`, `{"mes\u0073ages":` + a + `}`,
		`{"messages":[{"data":{"payload":null,"rayId":"r"}}]}`,
		`{"messages":[{"id":"1-0","data":` + observationData() + `,"DATA":{"payload":"{}"}}]}`,
		`{"messages":[{"id":"1-0","data":{"JobID":"j","TYPE":"x","PAYLOAD":"{}"}}]}`,
		`{"messages":[{"id":"1-0","data":{"payload":"\ud800"}}]}`,
		`{"messages":[{"id":"1-0","data":{"payload":7,"type":"partial"}}]}`,
		`{"messages":[{"id":7,"data":{"type":"partial"}}]}`,
		`{"messages":[{"data":[]}]}`, `{"messages":{}}`, `{"messages":`,
		`{"dispositions":[{"type":"terminal_duplicate","dispatchId":"ignored"}]}`,
		observationResponse(strings.Replace(observationData(), `"leaseEpoch":"9007199254740991"`, `"leaseEpoch":1`, 1)),
		observationResponse(strings.Replace(observationData(), `"claimToken":"`+observationClaim+`"`, `"claimToken":null`, 1)),
		`{"messages":[{"id":"1-0","data":{"type":"x","payload":"` + string([]byte{0xff}) + `"}}]}`,
	}
	for _, name := range []string{"messages", "Messages", "meſſageſ", `me\u017f\u017fage\u017f`} {
		for _, pair := range [][2]string{{a, b}, {b, a}, {"null", a}, {a, "null"}} {
			fixtures = append(fixtures, `{"messages":`+pair[0]+`,"`+name+`":`+pair[1]+`}`)
		}
	}
	for index, body := range fixtures {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			seed := func() ConsumeResponse {
				return ConsumeResponse{Messages: []StreamMessage{{ID: "seed", Data: StreamMessageData{JobID: "seed", Type: "seed", Payload: "{}", RayID: "seed"}}}}
			}
			want, got := seed(), seed()
			wantErr := json.Unmarshal([]byte(body), &want)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer srv.Close()
			c := NewClient(ClientConfig{BaseURL: srv.URL})
			defer c.Close()
			gotErr := c.doRequest(context.Background(), http.MethodPost, "/api/fabric/redis/jobs/consume", ConsumeRequest{}, &got)
			wantWire, _ := json.Marshal(want)
			gotWire, _ := json.Marshal(got)
			if string(wantWire) != string(gotWire) {
				t.Fatal("legacy public state/merge changed")
			}
			if wantErr == nil {
				if gotErr != nil {
					t.Fatal(gotErr)
				}
			} else {
				if gotErr == nil || errors.Unwrap(gotErr) == nil || errors.Unwrap(gotErr).Error() != wantErr.Error() {
					t.Fatal("decoder error changed")
				}
				if !reflect.DeepEqual(errors.Unwrap(gotErr), wantErr) {
					t.Fatal("type/field/offset/partial decoder error changed")
				}
			}
		})
	}
}

func TestDeliveryObservationManualProvenanceAndRetention(t *testing.T) {
	var resp ConsumeResponse
	if err := json.Unmarshal([]byte(observationResponse(observationData())), &resp); err != nil {
		t.Fatal(err)
	}
	job, err := ParseStreamMessage(resp.Messages[0])
	if err != nil {
		t.Fatal(err)
	}
	if job.DeliveryObservation().State() != deliverymetadata.Unknown {
		t.Fatal("manual HTTP decode fabricates provenance")
	}
	resp.Messages[0].Data.observation = deliverymetadata.CaptureHTTP([]byte(observationResponse(observationData())))
	original := resp.Messages[0].Data.observation
	if err := json.Unmarshal([]byte(`{"jobId":"replacement","type":"OTHER"}`), &resp.Messages[0].Data); err != nil {
		t.Fatal(err)
	}
	if resp.Messages[0].Data.observation != original || original.Identity().JobID != observationJobID || original.State() != deliverymetadata.Observed {
		t.Fatal("manual decode changed original snapshot")
	}
	parsed, err := ParseStreamMessage(resp.Messages[0])
	if err != nil {
		t.Fatal(err)
	}
	if parsed.DeliveryObservation().Identity().JobID != observationJobID {
		t.Fatal("conversion rebound original snapshot")
	}
	for _, typ := range []reflect.Type{reflect.TypeFor[StreamMessageData](), reflect.TypeFor[Job](), reflect.TypeFor[WSMessage]()} {
		field, ok := typ.FieldByName("observation")
		if !ok || field.PkgPath == "" || field.Type.Kind() != reflect.Pointer || field.Tag.Get("json") != "-" {
			t.Fatal("private pointer/JSON tag contract changed")
		}
	}
}

func TestDeliveryObservationExistingRawHTTPErrorExposure(t *testing.T) {
	body := `{"claimToken":"` + observationClaim + `","messages":`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL})
	defer c.Close()
	_, err := c.ConsumeJob(context.Background(), ConsumeRequest{})
	if err == nil || !strings.Contains(err.Error(), observationClaim) {
		t.Fatal("baseline raw malformed-body error exposure changed")
	}
}

func TestDeliveryObservationWSActualReadLoop(t *testing.T) {
	ready := make(chan struct{})
	finish := make(chan struct{})
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		select {
		case <-ready:
		case <-finish:
			return
		}
		// Numeric metadata still fails the existing map[string]string decoder.
		conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"job","data":{"leaseEpoch":1}}`))
		conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"terminal_duplicate","data":{}}`))
		conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"job","queue":"wire-queue","id":"1-0","data":`+observationData()+`}`))
		<-finish
	}))
	defer srv.Close()
	c := NewWSClient(WSClientConfig{BaseURL: srv.URL})
	defer c.Close()
	defer close(finish)
	jobs := make(chan WSMessage, 2)
	c.OnMessage("job", func(msg WSMessage) { jobs <- msg })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	close(ready)
	select {
	case msg := <-jobs:
		o := msg.DeliveryObservation()
		if o.State() != deliverymetadata.Observed || o.Identity().Queue != "wire-queue" {
			t.Fatal("readLoop capture missing")
		}
		msg.Data["jobId"] = "mutation"
		msg.Data["claimToken"] = "mutation"
		msg.Queue = "mutation"
		msg.ID = "mutation"
		if o.Identity().JobID != observationJobID || o.Identity().Queue != "wire-queue" || o.Identity().MessageID != "1-0" {
			t.Fatal("original frame identity rebound")
		}
		// Existing Data marshaling remains public. Compare to the same map
		// with no private slot; do not claim the original wire map is scrubbed.
		baseline := msg
		baseline.observation = nil
		a, _ := json.Marshal(msg)
		b, _ := json.Marshal(baseline)
		if string(a) != string(b) {
			t.Fatal("WS representation changed")
		}
		manual := WSMessage{Data: map[string]string{"protocol": "durable-results-v1", "claimToken": observationClaim}}
		if manual.DeliveryObservation().State() != deliverymetadata.Unknown {
			t.Fatal("map-only provenance invented")
		}
	case <-ctx.Done():
		t.Fatal("job not delivered")
	}
	select {
	case <-jobs:
		t.Fatal("malformed/disposition delivered as job")
	default:
	}
}

func TestDeliveryObservationParentFormats(t *testing.T) {
	o := deliverymetadata.CaptureHTTP([]byte(observationResponse(observationData())))
	parents := []any{StreamMessageData{observation: o}, Job{observation: o}, WSMessage{observation: o}, []Job{{observation: o}}, map[string]Job{"entry": {observation: o}}}
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%d", "%f", "%z", "%#020.12f"} {
		for _, parent := range parents {
			out := fmt.Sprintf(format, parent)
			for _, value := range []string{observationClaim, base64.StdEncoding.EncodeToString([]byte(observationClaim)), observationJobID} {
				if strings.Contains(out, value) {
					t.Fatalf("new private format %s leaked", format)
				}
			}
		}
	}
	checkObservationMarshal(t, StreamMessageData{observation: o}, StreamMessageData{})
}

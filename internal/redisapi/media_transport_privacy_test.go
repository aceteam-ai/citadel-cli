package redisapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// This transport never dials, binds a listener or resolves a hostname.
type mediaResponseTransport struct{ body string }

func (m mediaResponseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(m.body))}, nil
}

func TestMediaTransportMalformedHTTPBeforeKindIsKnown(t *testing.T) {
	const value = "1e9999"
	var logs []string
	client := NewClient(ClientConfig{BaseURL: "https://fixture.invalid", DebugFunc: func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}})
	client.httpClient.Transport = mediaResponseTransport{body: `{"messages":[{"id":"1-0","data":{"jobId":` + value + `}}]}`}
	var response ConsumeResponse
	err := client.doRequest(context.Background(), http.MethodPost, "/api/fabric/redis/jobs/consume", ConsumeRequest{}, &response)
	var original *json.UnmarshalTypeError
	if err == nil || !errors.As(err, &original) {
		t.Fatal("HTTP delivery lost original decoder classification")
	}
	if strings.Contains(err.Error(), original.Error()) {
		t.Fatal("HTTP diagnostic exposed decoder-owned details")
	}
	for _, retained := range []string{err.Error(), fmt.Sprintf("%#v", struct{ err error }{err}), strings.Join(logs, "\n")} {
		if strings.Contains(retained, value) {
			t.Fatal("HTTP pre-kind diagnostic retained original decoder detail")
		}
	}
}

func TestMediaTransportWSDecodePreservesWireAndHidesMalformedFrames(t *testing.T) {
	const value = "1e9999"
	_, err := decodeWSDelivery([]byte(`{"type":"job","message":{"synthetic":` + value + `}}`))
	var original *json.UnmarshalTypeError
	if err == nil || !errors.As(err, &original) {
		t.Fatal("WS delivery lost original decoder classification")
	}
	if !strings.Contains(original.Error(), value) {
		t.Fatal("fixture did not reproduce original decoder value retention")
	}
	if strings.Contains(err.Error(), value) || strings.Contains(fmt.Sprintf("%#v", struct{ err error }{err}), value) {
		t.Fatal("WS pre-kind diagnostic retained original decoder detail")
	}
	rawPayload := " {\"inputs\":{\"capability\":\"synthetic-transport-token\"},\"n\":1e+03} \n"
	frame, err := json.Marshal(WSMessage{Type: "job", ID: "1-0", Data: map[string]string{"jobId": "synthetic", "type": "MEDIA_GENERATE", "payload": rawPayload}})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := decodeWSDelivery(frame)
	if err != nil || msg.Type != "job" || msg.ID != "1-0" || msg.Data["payload"] != rawPayload {
		t.Fatal("WS decoding normalized or changed original delivery")
	}
	job, err := ParseStreamMessage(StreamMessage{ID: msg.ID, Data: StreamMessageData{JobID: msg.Data["jobId"], Type: msg.Data["type"], Payload: msg.Data["payload"]}})
	if err != nil || job.PrivateMediaPayload() == nil || string(job.PrivateMediaPayload().OriginalPayload()) != rawPayload {
		t.Fatal("WS decoded wire lost private original-byte projection")
	}
}

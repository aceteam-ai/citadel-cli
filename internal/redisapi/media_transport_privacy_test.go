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
type mediaResponseTransport struct {
	body   string
	status int
}

func (m mediaResponseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	status := m.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(m.body))}, nil
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

func TestMediaHTTPConsumeRetentionBoundary(t *testing.T) {
	const secret = "synthetic-http-retention.secret"
	fixtures := []struct {
		name    string
		payload string
		private bool
		invalid bool
	}{
		{"inbox", ` {"inputs":{"capability":"` + secret + `"},"n":1e+03} `, true, false},
		{"workspace", `{"inputs":[{"path":"` + secret + `.png"}]}`, true, false},
		{"null", `{"inputs":null,"prompt":"` + secret + `"}`, true, false},
		{"wrong_type", `{"inputs":"` + secret + `"}`, true, false},
		{"escaped_key", `{"inpu\u0074s":{"capability":"` + secret + `"}}`, true, false},
		{"duplicate", `{"inputs":{"capability":"` + secret + `"},"inputs":null}`, true, false},
		{"oversized", `{"inputs":"` + strings.Repeat(secret, 3000) + `"}`, true, false},
		{"nonobject", `[{"inputs":"` + secret + `"}]`, true, true},
		{"malformed", `{"inputs":"` + secret, true, true},
		{"legacy_text", `{"prompt":"legacy-text"}`, false, false},
	}
	for _, fixture := range fixtures {
		for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusInternalServerError} {
			t.Run(fmt.Sprintf("%s/%d", fixture.name, status), func(t *testing.T) {
				var logs []string
				client := NewClient(ClientConfig{BaseURL: "https://fixture.invalid", DebugFunc: func(format string, args ...any) {
					logs = append(logs, fmt.Sprintf(format, args...))
				}})
				body, err := json.Marshal(ConsumeResponse{Messages: []StreamMessage{{ID: "1-0", Data: StreamMessageData{
					JobID: "fixture-job", Type: "MEDIA_GENERATE", Payload: fixture.payload,
				}}}})
				if err != nil {
					t.Fatal(err)
				}
				client.httpClient.Transport = mediaResponseTransport{body: string(body), status: status}
				var response ConsumeResponse
				err = client.doRequest(context.Background(), http.MethodPost, "/api/fabric/redis/jobs/consume", ConsumeRequest{}, &response)
				if client.LastConsumeStatus() != status {
					t.Fatal("consume status observation changed")
				}
				if status >= 400 {
					if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("status %d", status)) {
						t.Fatal("consume error lost its HTTP status")
					}
				} else {
					if err != nil || len(response.Messages) != 1 || response.Messages[0].Data.Payload != fixture.payload {
						t.Fatal("consume changed original nested wire bytes")
					}
					job, parseErr := ParseStreamMessage(response.Messages[0])
					if fixture.invalid {
						if parseErr == nil || job != nil {
							t.Fatal("malformed payload unexpectedly accepted")
						}
						err = parseErr
					} else {
						if parseErr != nil || job == nil || job.JobID != "fixture-job" || job.MessageID != "1-0" {
							t.Fatal("parsed consume identity changed")
						}
						if fixture.private {
							if job.PrivateMediaPayload() == nil || string(job.PrivateMediaPayload().OriginalPayload()) != fixture.payload {
								t.Fatal("private original-byte projection changed")
							}
							retained, marshalErr := json.Marshal(job)
							if marshalErr != nil || strings.Contains(string(retained), secret) || strings.Contains(fmt.Sprintf("%#v", struct{ job *Job }{job}), secret) {
								t.Fatal("parsed consume diagnostics retained private input")
							}
						} else if job.Payload["prompt"] != "legacy-text" || job.PrivateMediaPayload() != nil {
							t.Fatal("legacy text behavior changed")
						}
					}
				}
				retained := strings.Join(logs, "\n")
				if err != nil {
					retained += err.Error() + fmt.Sprintf("%#v", struct{ err error }{err})
				}
				if strings.Contains(retained, secret) {
					t.Fatal("consume response/error diagnostic retained private input")
				}
			})
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

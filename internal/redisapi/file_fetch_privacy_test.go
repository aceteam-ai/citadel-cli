package redisapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFileFetchConsumeDiagnosticsOmitDeliveryBodies(t *testing.T) {
	const token = "synthetic-delivery.synthetic-credential"
	for _, response := range []struct {
		name, body string
		status     int
	}{
		{"valid", `{"messages":[{"id":"1-0","data":{"jobId":"fixture","type":"FILE_FETCH","payload":"{\"capability\":\"` + token + `\"}"}}]}`, 200},
		{"malformed", `{"messages":["` + token + `"`, 200},
		{"wrong_type", `{"messages":"` + token + `"}`, 200},
		{"failure", `{"error":"` + token + `"}`, 500},
	} {
		t.Run(response.name, func(t *testing.T) {
			var diagnostics []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.WriteHeader(response.status)
				fmt.Fprint(w, response.body)
			}))
			defer srv.Close()
			client := NewClient(ClientConfig{BaseURL: srv.URL, Token: "synthetic-device", DebugFunc: func(format string, args ...any) { diagnostics = append(diagnostics, fmt.Sprintf(format, args...)) }})
			job, err := client.ConsumeJob(context.Background(), ConsumeRequest{Queue: "jobs:v1:fixture", Group: "workers"})
			if err != nil {
				diagnostics = append(diagnostics, err.Error())
			}
			if strings.Contains(strings.Join(diagnostics, "\n"), token) {
				t.Fatal("pre-handler response/error diagnostics retained delivery credential")
			}
			if response.name == "valid" && (err != nil || job == nil || job.Payload["capability"] != token) {
				t.Fatal("diagnostic omission changed in-flight execution payload")
			}
		})
	}
}

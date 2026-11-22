package jobprivacy_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	redisclient "github.com/aceteam-ai/citadel-cli/internal/redis"
	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

// Exercise the public envelope methods, rather than only the shared helper.
// Payload, original bytes and raw stream fields stay intact for execution.
func TestContainerEnvelopeDiagnosticsOmitPayloadWithoutMutation(t *testing.T) {
	for _, kind := range []string{
		"CONTAINER_CREATE", "CONTAINER_START", "CONTAINER_STOP", "CONTAINER_EXEC",
		"CONTAINER_FILE_LIST", "CONTAINER_FILE_READ", "CONTAINER_FILE_WRITE",
		"CONTAINER_USAGE", "CONTAINER_EXPORT_CHUNK", "CONTAINER_IMPORT_CHUNK",
		"CONTAINER_DESTROY", "CONTAINER_FUTURE", "FILE_FETCH",
	} {
		t.Run(kind, func(t *testing.T) {
			payload := map[string]any{"transfer_token": "synthetic-transfer-authority", "data": "synthetic-private-file-bytes"}
			raw := []byte(`{"transfer_token":"synthetic-raw-authority","data":"synthetic-raw-bytes"}`)
			rawData := map[string]any{"payload": string(raw), "other": "synthetic-raw-stream-field"}
			beforePayload, _ := json.Marshal(payload)
			beforeRaw := bytes.Clone(raw)
			beforeData, _ := json.Marshal(rawData)
			workerJob := worker.Job{Type: kind, Payload: payload, RawPayload: raw}
			redisJob := redisclient.Job{Type: kind, Payload: payload, RawPayload: raw, RawData: rawData}
			apiJob := redisapi.Job{Type: kind, Payload: payload, RawPayload: raw, RawData: rawData}
			for _, envelope := range []struct {
				name string
				job  any
			}{
				{"worker-value", workerJob}, {"worker-pointer", &workerJob},
				{"redis-value", redisJob}, {"redis-pointer", &redisJob},
				{"api-value", apiJob}, {"api-pointer", &apiJob},
			} {
				t.Run(envelope.name, func(t *testing.T) {
					t.Run("json", func(t *testing.T) {
						encoded, err := json.Marshal(envelope.job)
						if err != nil {
							t.Fatal(err)
						}
						var retained map[string]any
						if err := json.Unmarshal(encoded, &retained); err != nil {
							t.Fatal(err)
						}
						for _, key := range []string{"Payload", "payload", "RawPayload", "RawData", "raw_data"} {
							if retained[key] != nil {
								t.Fatalf("retained %s in diagnostic JSON", key)
							}
						}
						if retained["Type"] != kind && retained["type"] != kind {
							t.Fatal("safe envelope identity was lost")
						}
					})
					t.Run("format", func(t *testing.T) {
						for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d"} {
							got := fmt.Sprintf(format, envelope.job)
							if got != "<credential-bearing job; payload omitted>" && got != "<FILE_FETCH job; payload omitted>" {
								t.Errorf("format %s retained more than the static omission marker", format)
							}
						}
					})
				})
			}
			afterPayload, _ := json.Marshal(payload)
			afterData, _ := json.Marshal(rawData)
			if !bytes.Equal(beforePayload, afterPayload) || !bytes.Equal(beforeRaw, raw) || !bytes.Equal(beforeData, afterData) {
				t.Fatal("diagnostics mutated shared execution fields")
			}
			for _, job := range []struct {
				payload map[string]any
				raw     []byte
				data    map[string]any
			}{
				{workerJob.Payload, workerJob.RawPayload, nil},
				{redisJob.Payload, redisJob.RawPayload, redisJob.RawData},
				{apiJob.Payload, apiJob.RawPayload, apiJob.RawData},
			} {
				if !reflect.DeepEqual(job.payload, payload) || !bytes.Equal(job.raw, raw) || job.data != nil && !reflect.DeepEqual(job.data, rawData) {
					t.Fatal("diagnostics replaced the live execution envelope")
				}
			}
		})
	}
}

func TestOrdinaryEnvelopeDiagnosticsKeepPayload(t *testing.T) {
	payload := map[string]any{"safe_value": "ordinary-job-content"}
	for _, job := range []any{
		worker.Job{Type: "CLOCK", Payload: payload},
		redisclient.Job{Type: "CLOCK", Payload: payload},
		redisapi.Job{Type: "CLOCK", Payload: payload},
	} {
		encoded, err := json.Marshal(job)
		if err != nil || !strings.Contains(string(encoded), "ordinary-job-content") {
			t.Fatal("ordinary diagnostic JSON changed")
		}
		if !strings.Contains(fmt.Sprintf("%+v", job), "ordinary-job-content") {
			t.Fatal("ordinary formatted diagnostics changed")
		}
	}
}

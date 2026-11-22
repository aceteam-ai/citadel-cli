package jobprivacy

import (
	"errors"
	"strings"
	"testing"
)

func TestContainerDiagnosticsRetainNoPayloadOrReflectedSecrets(t *testing.T) {
	payload := map[string]any{"key_b64": "synthetic-secret", "chunk_urls": []string{"https://blob.example.invalid/private?token=synthetic"}, "content_b64": "eA==", "argv": []string{"synthetic-private"}}
	for _, kind := range []string{"CONTAINER_CREATE", "CONTAINER_EXEC", "CONTAINER_FILE_WRITE", "CONTAINER_EXPORT_CHUNK", "CONTAINER_IMPORT_CHUNK"} {
		if Payload(kind, payload) != nil {
			t.Fatal("payload retained")
		}
		source := errors.New("native failure synthetic-secret private?token=synthetic eA==")
		if got := Error(kind, payload, source); got == nil || strings.Contains(got.Error(), "synthetic-secret") || !errors.Is(got, source) {
			t.Fatal("reflected credential or lost classification")
		}
		if data := FailureData(kind, payload, source, map[string]any{"request": payload}); len(data) != 1 {
			t.Fatal("failure metadata retained payload")
		}
	}
}

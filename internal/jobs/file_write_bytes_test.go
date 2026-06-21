package jobs

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

func TestFileWriteBytes_MissingPath(t *testing.T) {
	h := NewFileWriteBytesHandler(t.TempDir())
	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "t1",
		Type:    "FILE_WRITE_BYTES",
		Payload: map[string]string{"content": "AAAA"},
	})
	if err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestFileWriteBytes_MissingContent(t *testing.T) {
	h := NewFileWriteBytesHandler(t.TempDir())
	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "t2",
		Type:    "FILE_WRITE_BYTES",
		Payload: map[string]string{"path": "audio.webm"},
	})
	if err == nil {
		t.Fatal("expected error for missing content")
	}
}

func TestFileWriteBytes_InvalidBase64(t *testing.T) {
	h := NewFileWriteBytesHandler(t.TempDir())
	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "t3",
		Type: "FILE_WRITE_BYTES",
		Payload: map[string]string{
			"path":    "audio.webm",
			"content": "not!valid!base64!",
		},
	})
	if err == nil {
		t.Fatal("expected error for invalid base64")
	}
}

func TestFileWriteBytes_Success(t *testing.T) {
	dir := t.TempDir()
	h := NewFileWriteBytesHandler(dir)

	// Binary payload with a NUL byte to prove binary-safety vs. text FILE_WRITE.
	raw := []byte{0x00, 0x01, 0x02, 0xff, 'h', 'i'}
	encoded := base64.StdEncoding.EncodeToString(raw)

	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "t4",
		Type: "FILE_WRITE_BYTES",
		Payload: map[string]string{
			"path":    "recordings/meeting.webm",
			"content": encoded,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("result not JSON: %v", err)
	}
	if bw, ok := res["bytes_written"].(float64); !ok || int(bw) != len(raw) {
		t.Errorf("bytes_written = %v, want %d", res["bytes_written"], len(raw))
	}

	// Verify the file on disk contains the exact raw bytes.
	written, err := os.ReadFile(filepath.Join(dir, "recordings", "meeting.webm"))
	if err != nil {
		t.Fatalf("reading written file: %v", err)
	}
	if string(written) != string(raw) {
		t.Errorf("written bytes = %v, want %v", written, raw)
	}
}

func TestFileWriteBytes_ExceedsMaxBytes(t *testing.T) {
	h := NewFileWriteBytesHandler(t.TempDir())
	raw := make([]byte, 100)
	encoded := base64.StdEncoding.EncodeToString(raw)

	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "t5",
		Type: "FILE_WRITE_BYTES",
		Payload: map[string]string{
			"path":      "big.bin",
			"content":   encoded,
			"max_bytes": "10",
		},
	})
	if err == nil {
		t.Fatal("expected error when decoded size exceeds max_bytes")
	}
}

func TestFileWriteBytes_PathEscapeRejected(t *testing.T) {
	h := NewFileWriteBytesHandler(t.TempDir())
	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "t6",
		Type: "FILE_WRITE_BYTES",
		Payload: map[string]string{
			"path":    "../../etc/evil",
			"content": base64.StdEncoding.EncodeToString([]byte("x")),
		},
	})
	if err == nil {
		t.Fatal("expected error for path escaping the workspace")
	}
}

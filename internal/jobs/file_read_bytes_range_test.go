package jobs

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fileReadRangeResult struct {
	ContractVersion int    `json:"contract_version"`
	Encoding        string `json:"encoding"`
	Content         string `json:"content"`
	Offset          int64  `json:"offset"`
	Size            int    `json:"size"`
	TotalSize       int64  `json:"total_size"`
	EOF             bool   `json:"eof"`
}

func runFileReadRange(t *testing.T, workspace string, payload map[string]string) fileReadRangeResult {
	t.Helper()
	output, err := NewFileReadBytesRangeHandler(workspace).Execute(JobContext{}, makeJob(payload))
	if err != nil {
		t.Fatalf("range read: %v", err)
	}
	var result fileReadRangeResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return result
}

func TestFileReadBytesRange_LargeSparseFileAndStableMetadata(t *testing.T) {
	workspace := setupWorkspace(t)
	path := filepath.Join(workspace, "large-video.mp4")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const totalSize int64 = 64*1024*1024 + 5
	if err := file.Truncate(totalSize); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("FINAL"), totalSize-5); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	first := runFileReadRange(t, workspace, map[string]string{
		"path": path, "offset": "0", "length": "8", "max_bytes": "8",
	})
	firstBytes, err := base64.StdEncoding.DecodeString(first.Content)
	if err != nil {
		t.Fatal(err)
	}
	if first.ContractVersion != fileReadRangeVersion || first.Encoding != "base64" ||
		first.Offset != 0 || first.Size != 8 || first.TotalSize != totalSize || first.EOF ||
		!bytes.Equal(firstBytes, make([]byte, 8)) {
		t.Fatalf("first range = %+v, bytes = %v", first, firstBytes)
	}

	last := runFileReadRange(t, workspace, map[string]string{
		"path": path, "offset": fmt.Sprint(totalSize - 5), "length": "8",
	})
	lastBytes, err := base64.StdEncoding.DecodeString(last.Content)
	if err != nil {
		t.Fatal(err)
	}
	if last.ContractVersion != fileReadRangeVersion || last.Offset != totalSize-5 ||
		last.Size != 5 || last.TotalSize != totalSize || !last.EOF || string(lastBytes) != "FINAL" {
		t.Fatalf("last range = %+v, bytes = %q", last, lastBytes)
	}

	atEOF := runFileReadRange(t, workspace, map[string]string{
		"path": path, "offset": fmt.Sprint(totalSize), "length": "8",
	})
	if atEOF.Offset != totalSize || atEOF.Size != 0 || atEOF.Content != "" ||
		atEOF.TotalSize != totalSize || !atEOF.EOF {
		t.Fatalf("EOF range = %+v", atEOF)
	}
}

func TestFileReadBytesRange_RejectsInvalidAndOverflowRanges(t *testing.T) {
	workspace := setupWorkspace(t)
	path := writeTestFile(t, workspace, "short.bin", "0123456789")
	handler := NewFileReadBytesRangeHandler(workspace)
	tests := []map[string]string{
		{"path": path},
		{"path": path, "offset": "0"},
		{"path": path, "length": "1"},
		{"path": path, "offset": "-1", "length": "1"},
		{"path": path, "offset": "0", "length": "0"},
		{"path": path, "offset": "11", "length": "1"},
		{"path": path, "offset": "9223372036854775808", "length": "1"},
		{"path": path, "offset": "0", "length": "9223372036854775808"},
		{"path": path, "offset": "0", "length": fmt.Sprint(maxReadChunkBytes + 1)},
		{"path": path, "offset": "0", "length": "2", "max_bytes": "1"},
		{"path": path, "offset": "0", "length": "1", "max_bytes": fmt.Sprint(maxReadChunkBytes + 1)},
	}
	for _, payload := range tests {
		if _, err := handler.Execute(JobContext{}, makeJob(payload)); err == nil {
			t.Errorf("payload %v: expected refusal", payload)
		}
	}
	if _, _, err := parseFileReadRange(map[string]string{
		"offset": fmt.Sprint(math.MaxInt64 - 1), "length": "8",
	}, math.MaxInt64); err == nil {
		t.Fatal("expected offset plus length overflow refusal")
	}
}

func TestFileReadBytesRange_LegacyFallbackKeepsWholeFileCap(t *testing.T) {
	workspace := setupWorkspace(t)
	path := filepath.Join(workspace, "large.bin")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(defaultMaxReadBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	legacy := NewFileReadBytesHandler(workspace)
	for _, payload := range []map[string]string{
		{"path": path, "offset": "0", "length": "1"},
		{"path": path, "max_bytes": fmt.Sprint(defaultMaxReadBytes + 1)},
		{"path": path, "offset": "0", "length": "1", "max_bytes": fmt.Sprint(defaultMaxReadBytes + 1)},
	} {
		if _, err := legacy.Execute(JobContext{}, makeJob(payload)); err == nil || !strings.Contains(err.Error(), "max_bytes") {
			t.Errorf("legacy payload %v: expected 50 MiB cap refusal, got %v", payload, err)
		}
	}
	result := runFileReadRange(t, workspace, map[string]string{
		"path": path, "offset": "0", "length": "1",
	})
	if result.Size != 1 || result.TotalSize != defaultMaxReadBytes+1 {
		t.Fatalf("versioned range = %+v", result)
	}
}

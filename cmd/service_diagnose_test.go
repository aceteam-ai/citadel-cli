package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/diagnose"
	"github.com/aceteam-ai/citadel-cli/internal/status"
)

func TestEnvToMap(t *testing.T) {
	env := []string{"FOO=bar", "EMPTY=", "NO_EQUALS", "DUP=first", "DUP=second"}
	m := envToMap(env)

	if m["FOO"] != "bar" {
		t.Fatalf("FOO = %q, want %q", m["FOO"], "bar")
	}
	if v, ok := m["EMPTY"]; !ok || v != "" {
		t.Fatalf("EMPTY = %q, ok=%v, want present and empty", v, ok)
	}
	if _, ok := m["NO_EQUALS"]; ok {
		t.Fatalf("NO_EQUALS should not produce a map entry (no '=')")
	}
	if m["DUP"] != "second" {
		t.Fatalf("DUP = %q, want last-write-wins %q", m["DUP"], "second")
	}
}

func TestEnvFileToMap(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "vllm.env")
	content := "# a comment\nVLLM_MODEL=BAAI/bge-large-en\n\nVLLM_API_KEY=sk-secret\n"
	if err := os.WriteFile(envPath, []byte(content), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	m := envFileToMap(envPath)
	if m["VLLM_MODEL"] != "BAAI/bge-large-en" {
		t.Fatalf("VLLM_MODEL = %q, want %q", m["VLLM_MODEL"], "BAAI/bge-large-en")
	}
	if m["VLLM_API_KEY"] != "sk-secret" {
		t.Fatalf("VLLM_API_KEY = %q, want %q", m["VLLM_API_KEY"], "sk-secret")
	}
	if _, ok := m["a comment"]; ok {
		t.Fatalf("comment line should not produce an entry")
	}
}

func TestEnvFileToMap_MissingFileYieldsEmptyMap(t *testing.T) {
	m := envFileToMap(filepath.Join(t.TempDir(), "does-not-exist.env"))
	if len(m) != 0 {
		t.Fatalf("expected an empty map for a missing file, got %v", m)
	}
	if m := envFileToMap(""); len(m) != 0 {
		t.Fatalf("expected an empty map for an empty path, got %v", m)
	}
}

func TestFreeVRAMMB(t *testing.T) {
	cases := []struct {
		name   string
		gpus   []status.GPUMetrics
		wantMB int
		wantOK bool
	}{
		{
			name:   "no GPUs",
			gpus:   nil,
			wantMB: 0,
			wantOK: false,
		},
		{
			name: "prefers MemoryFreeMB signal (#833)",
			gpus: []status.GPUMetrics{
				{MemoryTotalMB: 24576, MemoryUsedMB: 18000, MemoryFreeMB: 6300},
			},
			wantMB: 6300,
			wantOK: true,
		},
		{
			name: "falls back to derived total-used when MemoryFreeMB unset",
			gpus: []status.GPUMetrics{
				{MemoryTotalMB: 24576, MemoryUsedMB: 18000},
			},
			wantMB: 6576,
			wantOK: true,
		},
		{
			name: "sums across multiple GPUs",
			gpus: []status.GPUMetrics{
				{MemoryTotalMB: 24576, MemoryFreeMB: 6300},
				{MemoryTotalMB: 24576, MemoryFreeMB: 20000},
			},
			wantMB: 26300,
			wantOK: true,
		},
		{
			name: "ignores a GPU with no reported total",
			gpus: []status.GPUMetrics{
				{MemoryTotalMB: 0},
			},
			wantMB: 0,
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mb, ok := freeVRAMMB(c.gpus)
			if mb != c.wantMB || ok != c.wantOK {
				t.Fatalf("freeVRAMMB() = (%d, %v), want (%d, %v)", mb, ok, c.wantMB, c.wantOK)
			}
		})
	}
}

func TestRenderDiagnoseJSON(t *testing.T) {
	r := diagnose.Diagnose(diagnose.Input{
		ServiceName: "vllm",
		Managed:     true,
		Container:   diagnose.ContainerState{Exists: true, Name: "citadel-vllm", Status: "exited", ExitCode: 1},
	})

	var buf bytes.Buffer
	if err := renderDiagnoseJSON(&buf, r); err != nil {
		t.Fatalf("renderDiagnoseJSON() error: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	for _, key := range []string{"service", "managed", "container", "vram", "most_likely_cause", "suggested_action"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("JSON report missing key %q: %v", key, decoded)
		}
	}
	if decoded["service"] != "vllm" {
		t.Fatalf(`decoded["service"] = %v, want "vllm"`, decoded["service"])
	}
}

func TestRenderDiagnoseReport_DoesNotPanicOnEmptyReport(t *testing.T) {
	r := diagnose.Diagnose(diagnose.Input{ServiceName: "unmanaged-thing"})
	var buf bytes.Buffer
	renderDiagnoseReport(&buf, r) // must not panic
	out := buf.String()
	if !strings.Contains(out, "unmanaged-thing") {
		t.Fatalf("rendered report doesn't mention the service name: %s", out)
	}
	if !strings.Contains(out, "DIAGNOSIS") {
		t.Fatalf("rendered report missing DIAGNOSIS section: %s", out)
	}
}

func TestRunServiceDiagnose_RejectsInvalidServiceName(t *testing.T) {
	err := runServiceDiagnose(serviceDiagnoseCmd, []string{"../../etc/passwd"})
	if err == nil {
		t.Fatalf("expected an error for an invalid service name")
	}
}

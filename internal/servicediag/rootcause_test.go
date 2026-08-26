package servicediag

import (
	"strings"
	"testing"
)

func TestExtractRootError_PythonTraceback(t *testing.T) {
	lines := []string{
		"INFO: starting engine",
		"Traceback (most recent call last):",
		`  File "server.py", line 42, in load_model`,
		"    model = AutoModel.from_pretrained(path)",
		"ValueError: The checkpoint you are trying to load has model type `newmodel` but Transformers does not recognize this architecture.",
	}
	got := ExtractRootError(lines)
	want := "ValueError: The checkpoint you are trying to load has model type `newmodel` but Transformers does not recognize this architecture."
	if got != want {
		t.Errorf("ExtractRootError() = %q, want %q", got, want)
	}
}

func TestExtractRootError_LastNonEmptyLineFallback(t *testing.T) {
	lines := []string{
		"INFO: starting up",
		"INFO: still going",
		"",
		"process exited unexpectedly",
		"",
	}
	got := ExtractRootError(lines)
	want := "process exited unexpectedly"
	if got != want {
		t.Errorf("ExtractRootError() = %q, want %q", got, want)
	}
}

func TestExtractRootError_Empty(t *testing.T) {
	if got := ExtractRootError(nil); got != "" {
		t.Errorf("ExtractRootError(nil) = %q, want empty", got)
	}
	if got := ExtractRootError([]string{"", "  ", ""}); got != "" {
		t.Errorf("ExtractRootError(blank lines) = %q, want empty", got)
	}
}

func TestExtractRootError_GenericErrorPrefix(t *testing.T) {
	lines := []string{
		"model loading...",
		"Error: failed to bind to port 8000: address already in use",
	}
	got := ExtractRootError(lines)
	want := "Error: failed to bind to port 8000: address already in use"
	if got != want {
		t.Errorf("ExtractRootError() = %q, want %q", got, want)
	}
}

func TestErrorHints(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  string // substring expected in at least one hint; "" means expect nil
	}{
		{"oom", []string{"torch.cuda.OutOfMemoryError: CUDA out of memory."}, "out-of-memory"},
		{"trust_remote_code", []string{"ValueError: Loading this model requires you to execute custom code: set `trust_remote_code=True`"}, "trust_remote_code"},
		{"model_not_supported", []string{"ValueError: model type NewModel not supported"}, "architecture may not be supported"},
		{"port_in_use", []string{"bind: address already in use"}, "port already in use"},
		{"permission_denied", []string{"open /models/foo: permission denied"}, "permission error"},
		{"no_match", []string{"INFO: server ready on :8000"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hints := ErrorHints(tt.lines)
			if tt.want == "" {
				if len(hints) != 0 {
					t.Errorf("ErrorHints() = %v, want none", hints)
				}
				return
			}
			found := false
			for _, h := range hints {
				if strings.Contains(h, tt.want) {
					found = true
				}
			}
			if !found {
				t.Errorf("ErrorHints() = %v, want a hint containing %q", hints, tt.want)
			}
		})
	}
}

func TestErrorHints_Empty(t *testing.T) {
	if hints := ErrorHints(nil); hints != nil {
		t.Errorf("ErrorHints(nil) = %v, want nil", hints)
	}
}

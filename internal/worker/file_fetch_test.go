package worker

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

func TestFileFetchCapabilityRequiresWorkspaceFilesAndOrigin(t *testing.T) {
	for _, tc := range []struct {
		name      string
		workspace bool
		files     bool
		origin    string
		want      bool
	}{
		{"enabled", true, true, "https://example.com", true},
		{"disabled_files", true, false, "https://example.com", false},
		{"missing_workspace", false, true, "https://example.com", false},
		{"missing_origin", true, true, "", false},
		{"http_origin", true, true, "http://example.com", false},
		{"non_origin", true, true, "https://example.com/path", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := ""
			if tc.workspace {
				workspace = t.TempDir()
			}
			handlers := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{
				WorkspaceDir: workspace, FilesDisabled: !tc.files,
				FileFetchOrigin: func() string { return tc.origin },
			})
			if got := anyHandles(handlers, JobTypeFileFetch); got != tc.want {
				t.Fatalf("FILE_FETCH available=%t want=%t", got, tc.want)
			}
			runner := NewRunner(nil, handlers, RunnerConfig{})
			advertised := false
			for _, typ := range runner.SupportedJobTypes() {
				if typ == JobTypeFileFetch {
					advertised = true
				}
			}
			if advertised != tc.want {
				t.Fatalf("FILE_FETCH advertised=%t want=%t", advertised, tc.want)
			}
		})
	}
}

func TestFileFetchAdapterBoundedNumericMaxBytes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  string
	}{
		{"json_number", float64(536870912), "workspace_unavailable"},
		{"decimal_string", "536870912", "workspace_unavailable"},
		{"ceiling", float64(2147483648), "workspace_unavailable"},
		{"fraction", 536870912.5, "invalid_max_bytes"},
		{"scientific_fraction", "5.368709125e8", "invalid_max_bytes"},
		{"scientific_string", "5.36870912e8", "invalid_max_bytes"},
		{"nan", math.NaN(), "invalid_max_bytes"},
		{"infinity", math.Inf(1), "invalid_max_bytes"},
		{"negative", float64(-1), "invalid_max_bytes"},
		{"zero", float64(0), "invalid_max_bytes"},
		{"over_cap", float64(2147483649), "invalid_max_bytes"},
		{"overflow", 1e80, "invalid_max_bytes"},
		{"malformed", "many bytes", "invalid_max_bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := jobs.NewFileFetchHandler("", func() string { return "https://example.com" })
			adapter := NewLegacyHandlerAdapter(JobTypeFileFetch, handler)
			_, err := adapter.Execute(context.Background(), &Job{Type: JobTypeFileFetch, Payload: map[string]any{
				"url":       "https://example.com" + jobs.FileFetchEndpointPrefix + "body.signature",
				"dest_path": "film.mp4", "expected_sha256": strings.Repeat("0", 64), "max_bytes": tc.value,
			}}, &NoOpStreamWriter{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want=%s", err, tc.want)
			}
		})
	}
	legacy := &TestLegacyHandler{}
	adapter := NewLegacyHandlerAdapter(JobTypeFileWriteBytes, legacy)
	_, err := adapter.Execute(context.Background(), &Job{Type: JobTypeFileWriteBytes, Payload: map[string]any{"max_bytes": float64(536870912)}}, &NoOpStreamWriter{})
	if err != nil || legacy.capturedPayload["max_bytes"] != "5.36870912e+08" {
		t.Fatalf("changed unrelated adapter semantics: %v %+v", err, legacy.capturedPayload)
	}
}

func TestFileFetchCapabilityFollowsLivePermissionAndOrigin(t *testing.T) {
	files := false
	origin := "https://example.com"
	handlers := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{
		WorkspaceDir: t.TempDir(), FilesDisabled: true,
		FilesEnabled: func() bool { return files }, FileFetchOrigin: func() string { return origin },
	})
	if anyHandles(handlers, JobTypeFileFetch) {
		t.Fatal("disabled Files advertised")
	}
	files = true
	if !anyHandles(handlers, JobTypeFileFetch) {
		t.Fatal("enabled Files not advertised")
	}
	var selected JobHandler
	for _, handler := range handlers {
		if handler.CanHandle(JobTypeFileFetch) {
			selected = handler
		}
	}
	if selected == nil {
		t.Fatal("missing handler")
	}
	files = false
	if anyHandles(handlers, JobTypeFileFetch) {
		t.Fatal("revoked Files advertised")
	}
	if _, err := selected.Execute(context.Background(), &Job{Type: JobTypeFileFetch}, &NoOpStreamWriter{}); err == nil {
		t.Fatal("selection/execution permission race allowed")
	}
	files = true
	origin = "http://example.com"
	if anyHandles(handlers, JobTypeFileFetch) {
		t.Fatal("invalid live origin advertised")
	}
	origin = "https://example.com"
	if !anyHandles(handlers, JobTypeFileFetch) {
		t.Fatal("restored live origin not advertised")
	}
	adapter := selected.(*LegacyHandlerAdapter)
	cap, ok := adapter.DeclaredCapabilities(JobTypeFileFetch)
	if !ok || !cap.CooperativeCancel || cap.ReceiptOwner != jobs.ReceiptOwnerNone {
		t.Fatalf("unexpected lifecycle contract: %+v", cap)
	}
}

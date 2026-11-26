package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

// refusingTmuxSessions keeps every cmd test away from the real tmux binary.
type refusingTmuxSessions struct{}

func (refusingTmuxSessions) Execute(jobs.JobContext, *nexus.Job) ([]byte, error) {
	return nil, errors.New("tmux is not available to cmd tests")
}

type recordingTmuxSessions struct{ calls int }

func (r *recordingTmuxSessions) Execute(jobs.JobContext, *nexus.Job) ([]byte, error) {
	r.calls++
	return []byte(`{"action":"list"}`), nil
}

func tmuxSessionRunner(t *testing.T, orgID string, shell *bool) *worker.Runner {
	t.Helper()
	handlers, _ := buildNodeJobHandlers(nodeJobHandlerOpts{
		OrgID: orgID, NodeID: "42", WorkspaceDir: t.TempDir(), PermissionsDir: t.TempDir(),
		ShellDisabled: !*shell, ShellEnabled: func() bool { return *shell },
		ShellPasscodeState: func() (jobs.PasscodeState, error) { return jobs.PasscodeState{}, nil },
		PasscodeThrottle:   jobs.NewPasscodeThrottle(),
		DesktopDisabled:    true, FilesDisabled: true,
		HandlerLog: func(string, ...any) {},
	})
	return worker.NewRunner(nil, handlers, worker.RunnerConfig{})
}

func TestTmuxSessionAdvertisedOnlyWithShell(t *testing.T) {
	shell := true
	runner := tmuxSessionRunner(t, "fixture-org", &shell)
	if !slices.Contains(runner.SupportedJobTypes(), worker.JobTypeTmuxSession) {
		t.Fatal("not advertised with the shell gate wired and shell on")
	}
	shell = false
	if slices.Contains(runner.SupportedJobTypes(), worker.JobTypeTmuxSession) {
		t.Fatal("advertised with shell off")
	}
	shell = true
	if slices.Contains(tmuxSessionRunner(t, "", &shell).SupportedJobTypes(), worker.JobTypeTmuxSession) {
		t.Fatal("advertised without an organization for the per-node stream")
	}
}

func TestTmuxSessionNotInDiagnosticHandlers(t *testing.T) {
	if _, ok := jobHandlers[worker.JobTypeTmuxSession]; ok {
		t.Fatal("TMUX_SESSION is registered on the diagnostic path, which has no per-node stream")
	}
}

// TestTmuxSessionUsesDeviceOrgStream: when the device and manifest orgs
// differ, the handler takes the device org, as the per-node queue does, and
// refuses the manifest org's stream.
func TestTmuxSessionUsesDeviceOrgStream(t *testing.T) {
	deviceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(deviceDir, "config.yaml"), []byte("device_api_token: fixture-token\norg_id: device-org\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withHookOverrides(t, deviceDir, func() {})
	manifestDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(manifestDir, "citadel.yaml"), []byte("node:\n  name: fixture\n  org_id: manifest-org\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CITADEL_NODE_DIR", manifestDir)
	if manifest, _, err := findAndReadManifest(); err != nil || manifest.Node.OrgID != "manifest-org" {
		t.Fatalf("manifest fixture not read: %v", err)
	}

	org := nodeJobOrgID()
	if org != "device-org" {
		t.Fatalf("nodeJobOrgID() = %q, want the device org", org)
	}
	sessions := &recordingTmuxSessions{}
	h := worker.NewTmuxSessionHandler(worker.TmuxSessionConfig{
		OrgID: org, NodeID: "42",
		Gate: jobs.ShellGate{
			Enabled:  func() bool { return true },
			State:    func() (jobs.PasscodeState, error) { return jobs.PasscodeState{}, nil },
			Throttle: jobs.NewPasscodeThrottle(),
		},
		Sessions: sessions,
	})
	job := func(queue string) *worker.Job {
		return &worker.Job{ID: "job-1", Type: worker.JobTypeTmuxSession, Source: "redis-api", SourceQueue: queue,
			Payload: map[string]any{"action": "list", "node_id": "42"}}
	}
	if _, err := h.Execute(context.Background(), job(nodeQueueName("manifest-org", "42")), nil); err == nil {
		t.Fatal("accepted the manifest org's stream")
	}
	if sessions.calls != 0 {
		t.Fatal("a refused job reached tmux")
	}
	if _, err := h.Execute(context.Background(), job(nodeQueueName(org, "42")), nil); err != nil {
		t.Fatalf("refused the subscribed per-node stream: %v", err)
	}
	if sessions.calls != 1 {
		t.Fatalf("tmux calls = %d, want 1", sessions.calls)
	}
}

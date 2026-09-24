package cmd

import (
	"sync/atomic"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

func TestStartStatusPublisherAfterRunnerPublishesLiveCapabilities(t *testing.T) {
	runner := worker.NewRunner(nil, worker.CreateLegacyHandlersWithOpts(worker.LegacyHandlerOpts{
		ShellDisabled: true,
		GOOS:          "linux",
	}), worker.RunnerConfig{})
	var stored atomic.Pointer[worker.Runner]
	called := false
	startStatusPublisherAfterRunner(&stored, runner, func() {
		called = true
		live := stored.Load()
		if live == nil {
			t.Fatal("initial publisher started before runner was stored")
		}
		for _, jt := range live.SupportedJobTypes() {
			if jt == worker.JobTypeShellCommand || jt == worker.JobTypeIOSBuild {
				t.Fatalf("initial capabilities advertise non-dispatchable %s", jt)
			}
		}
	})
	if !called {
		t.Fatal("status publisher was not started after runner registration")
	}
}

func TestControlCenterInitialHeartbeatIncludesPrivilegedHandlers(t *testing.T) {
	opts := nodeJobHandlerOpts{
		WorkspaceDir:  t.TempDir(),
		ShellDisabled: true,
		HandlerLog:    func(string, ...any) {},
	}
	handlers, _ := buildNodeJobHandlers(opts)
	runner := worker.NewRunner(nil, handlers, worker.RunnerConfig{})
	registerPrivilegedNodeJobHandlers(runner, opts)

	var stored atomic.Pointer[worker.Runner]
	collector := status.NewCollector(status.CollectorConfig{JobTypes: func() []string {
		if live := stored.Load(); live != nil {
			return live.SupportedJobTypes()
		}
		return nil
	}})
	startStatusPublisherAfterRunner(&stored, runner, func() {
		initial, err := collector.Collect()
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		got := make(map[string]bool, len(initial.Capabilities.JobTypes))
		for _, jobType := range initial.Capabilities.JobTypes {
			got[jobType] = true
		}
		for _, jobType := range []string{worker.JobTypeAgentUpdate, worker.JobTypeWhatsAppProvision} {
			if !got[jobType] {
				t.Fatalf("initial control-center capabilities omit privileged handler %s: %v", jobType, initial.Capabilities.JobTypes)
			}
		}
	})
}

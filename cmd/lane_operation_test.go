package cmd

import (
	"context"
	"os"
	"runtime"
	"slices"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/laneop"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

func laneOperationAdvertised(t *testing.T, run laneop.Exec, shell bool) bool {
	t.Helper()
	handlers, _ := buildNodeJobHandlers(nodeJobHandlerOpts{
		OrgID: "fixture-org", NodeID: "42", WorkspaceDir: t.TempDir(), PermissionsDir: t.TempDir(),
		ShellDisabled: !shell, ShellEnabled: func() bool { return shell },
		ShellPasscodeState: func() (jobs.PasscodeState, error) { return jobs.PasscodeState{}, nil },
		PasscodeThrottle:   jobs.NewPasscodeThrottle(),
		DesktopDisabled:    true, FilesDisabled: true,
		HandlerLog: func(string, ...any) {},
		LaneExec:   run,
	})
	runner := worker.NewRunner(nil, handlers, worker.RunnerConfig{})
	return slices.Contains(runner.SupportedJobTypes(), worker.JobTypeLaneOperation)
}

func TestLaneOperationAdvertisedOnlyWithTmuxAndShell(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() != os.Geteuid() {
		if laneOperationAdvertised(t, func(context.Context, []byte, ...string) ([]byte, error) { return []byte("tmux 3.4\n"), nil }, true) {
			t.Fatal("advertised on an unsupported platform")
		}
		return
	}
	calls := 0
	missing := func(context.Context, []byte, ...string) ([]byte, error) {
		calls++
		return nil, laneop.ErrTmuxUnavailable
	}
	if laneOperationAdvertised(t, missing, true) || calls == 0 {
		t.Fatalf("advertised without tmux (probe calls %d)", calls)
	}
	present := func(_ context.Context, _ []byte, args ...string) ([]byte, error) {
		if len(args) == 1 && args[0] == "-V" {
			return []byte("tmux 3.4\n"), nil
		}
		t.Fatalf("advertisement ran a tmux command: %v", args)
		return nil, nil
	}
	if !laneOperationAdvertised(t, present, true) {
		t.Fatal("not advertised with tmux and the shell gate wired")
	}
	if laneOperationAdvertised(t, present, false) {
		t.Fatal("advertised with shell disabled")
	}
}

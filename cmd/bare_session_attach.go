package cmd

import (
	"context"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/tui/sessionview"
	"github.com/aceteam-ai/citadel-cli/internal/worklock"
	"github.com/spf13/cobra"
)

// Dependencies belong to one invocation. No command/context/global remembers a
// previous decision, target directory, observation or startup authorization.
type bareSessionDependencies struct {
	resolveStateDir func() (string, error)
	request         sessionStatusRequester
	probeContention func(string) (bool, error)
	observePresence func(string) (bool, error)
	startup         func(*cobra.Command, []string)
	dispatch        func(*cobra.Command, bool, bool)
	runViewer       func(context.Context, string, sessionview.Snapshot) error
}

func productionBareSessionDependencies() bareSessionDependencies {
	return bareSessionDependencies{
		resolveStateDir: network.ObserveStateDir,
		request:         network.SessionControlStatusRequest,
		probeContention: worklock.ProbeContention,
		observePresence: network.ObserveStatePresence,
		startup:         prepareAgentInvocation,
		dispatch:        dispatchBareCitadel,
		runViewer: func(ctx context.Context, stateDir string, status sessionview.Snapshot) error {
			return runObservedSessionAttach(ctx, stateDir, status, network.SessionControlStatusRequest, runSessionViewProgram)
		},
	}
}

// runBareSessionSelection is observational until every absence prerequisite has
// succeeded. None is a snapshot, not a lease: the existing foreground Acquire
// remains authority, and unchanged legacy startup may re-resolve state later.
func runBareSessionSelection(cmd *cobra.Command, args []string, dependencies bareSessionDependencies) error {
	if cmd.Context().Err() != nil {
		return errSessionViewUnavailable
	}
	stateDir, err := dependencies.resolveStateDir()
	if err != nil || cmd.Context().Err() != nil {
		return errSessionViewUnavailable
	}
	status, absent, err := observeSessionViewStatus(cmd.Context(), stateDir, dependencies.request)
	if err != nil || cmd.Context().Err() != nil {
		return errSessionViewUnavailable
	}
	if !absent {
		return dependencies.runViewer(cmd.Context(), stateDir, status)
	}
	held, err := dependencies.probeContention(stateDir)
	if err != nil || held || cmd.Context().Err() != nil {
		return errSessionViewUnavailable
	}
	hasMeshState, err := dependencies.observePresence(stateDir)
	if err != nil || cmd.Context().Err() != nil {
		return errSessionViewUnavailable
	}
	dependencies.startup(cmd, args)
	dependencies.dispatch(cmd, true, hasMeshState)
	return nil
}

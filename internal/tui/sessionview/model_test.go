package sessionview

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func fixtureStatus() Snapshot {
	return Snapshot{Mode: "worker", Connected: true, PID: 123, UptimeSec: 10, Version: "fixture", NodeName: "owned-node", MeshIP: "100.64.0.1"}
}

func TestViewerSequentialPollAndDetachOnly(t *testing.T) {
	for _, key := range []tea.KeyMsg{{Type: tea.KeyCtrlC}, {Type: tea.KeyEsc}, {Type: tea.KeyRunes, Runes: []rune{'q'}}} {
		calls := 0
		var requestContext context.Context
		model := New(context.Background(), func(ctx context.Context) (Snapshot, error) {
			calls++
			requestContext = ctx
			return fixtureStatus(), nil
		})
		command := model.Init()
		if command == nil || model.Init() != nil {
			t.Fatal("initial observation must permit only one in-flight request")
		}
		if _, next := model.Update(command()); next == nil {
			t.Fatal("successful observation did not schedule its delayed refresh")
		}
		if calls != 1 || !strings.Contains(model.View(), "owned-node") || !strings.Contains(model.View(), "Mesh: connected") {
			t.Fatal("viewer did not render the real observed owner")
		}
		_, quit := model.Update(key)
		if quit == nil || !model.stopped || requestContext.Err() != context.Canceled {
			t.Fatal("detach did not stop only the viewer's request context")
		}
		if _, command := model.Update(refreshMsg{}); command != nil || calls != 1 {
			t.Fatal("detached viewer scheduled another observation")
		}
		model.Close() // Idempotent terminal/program cleanup.
	}
}

func TestViewerRefreshHasNoOverlapAndUsesFreshResult(t *testing.T) {
	calls := 0
	model := New(context.Background(), func(context.Context) (Snapshot, error) {
		calls++
		status := fixtureStatus()
		status.UptimeSec += int64(calls)
		return status, nil
	})
	defer model.Close()
	if _, next := model.Update(model.Init()()); next == nil {
		t.Fatal("first success did not return its delayed-refresh command")
	}
	_, poll := model.Update(refreshMsg{}) // Deliver the tick without sleeping it.
	if poll == nil {
		t.Fatal("refresh did not start the next observation")
	}
	if _, duplicate := model.Update(refreshMsg{}); duplicate != nil || calls != 1 {
		t.Fatal("refresh while pending started an overlapping observation")
	}
	if _, next := model.Update(poll()); next == nil || calls != 2 || model.status.UptimeSec != 12 {
		t.Fatal("second success reused stale data or failed to schedule its next delay")
	}
}

func TestViewerCancellationDoesNotAffectAnotherViewer(t *testing.T) {
	contexts := make([]context.Context, 2)
	models := make([]*Model, 2)
	for index := range models {
		index := index
		models[index] = New(context.Background(), func(ctx context.Context) (Snapshot, error) {
			contexts[index] = ctx
			return fixtureStatus(), nil
		})
		defer models[index].Close()
		models[index].Update(models[index].Init()())
	}
	models[0].Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if contexts[0].Err() != context.Canceled || contexts[1].Err() != nil || models[1].stopped {
		t.Fatal("detaching one viewer cancelled the other viewer")
	}
	_, poll := models[1].Update(refreshMsg{})
	if poll == nil {
		t.Fatal("remaining viewer could not keep observing its session")
	}
	models[1].Update(poll())
	if !models[1].haveStatus || contexts[1].Err() != nil {
		t.Fatal("remaining viewer lost its independent request context")
	}
}

func TestViewerClearsOldOwnerOnFailureOrObservedChange(t *testing.T) {
	for _, change := range []string{"error", "pid", "mode", "uptime"} {
		t.Run(change, func(t *testing.T) {
			status := fixtureStatus()
			model := New(context.Background(), func(context.Context) (Snapshot, error) { return status, nil })
			defer model.Close()
			model.Update(model.Init()())
			var err error
			switch change {
			case "error":
				err = errors.New("private-error-must-not-be-rendered")
			case "pid":
				status.PID++
			case "mode":
				status.Mode = "presence"
			case "uptime":
				status.UptimeSec--
			}
			model.Update(refreshMsg{})
			_, next := model.Update(statusMsg{generation: model.generation, snapshot: status, err: err})
			view := model.View()
			if next != nil || model.haveStatus || model.status != (Snapshot{}) || !strings.Contains(view, "unavailable or changed") {
				t.Fatal("failed/replaced session retained a healthy observation or automatic retry")
			}
			for _, private := range []string{"owned-node", "100.64.0.1", "Mesh: connected", "private-error-must-not-be-rendered"} {
				if strings.Contains(view, private) {
					t.Fatal("unavailable viewer retained previous owner or arbitrary error details")
				}
			}
		})
	}
}

func TestViewerLateObservationCannotReplaceCurrentState(t *testing.T) {
	model := New(context.Background(), func(context.Context) (Snapshot, error) { return fixtureStatus(), nil })
	defer model.Close()
	model.Init()
	model.Update(statusMsg{generation: model.generation + 1, snapshot: fixtureStatus()})
	if model.haveStatus || !model.pending {
		t.Fatal("foreign generation became a valid observation")
	}
	model.Close()
	model.Update(statusMsg{generation: model.generation - 1, snapshot: fixtureStatus()})
	if model.haveStatus {
		t.Fatal("late observation resurrected detached viewer")
	}
}

func TestViewerRendersTerminalStringsAsLiteralASCII(t *testing.T) {
	status := fixtureStatus()
	status.NodeName = "node\x1b[2J\n\r\u202e[red]"
	status.MeshIP = "\x9b31m"
	status.Version = "v\x07\t"
	model := New(context.Background(), func(context.Context) (Snapshot, error) { return status, nil })
	defer model.Close()
	model.Update(model.Init()())
	view := model.View()
	for _, control := range []string{"\x1b", "\r", "\u202e", "\x9b", "\x07", "\t"} {
		if strings.Contains(view, control) {
			t.Fatal("peer string injected a terminal control or bidi character")
		}
	}
	if !strings.Contains(view, `\x1b[2J\n\r\u202e[red]`) {
		t.Fatal("safe literal rendering lost the quoted owner text")
	}
}

func TestViewerAllowsObservedOfflineWithoutClaimingNodeExit(t *testing.T) {
	status := fixtureStatus()
	status.Connected = false
	model := New(context.Background(), func(context.Context) (Snapshot, error) { return status, nil })
	defer model.Close()
	model.Update(model.Init()())
	if !strings.Contains(model.View(), "Mesh: offline") || model.unavailable || !model.haveStatus {
		t.Fatal("valid offline status was confused with lost local control")
	}
}

func TestObservedViewerFencesItsFirstPollAgainstPreflight(t *testing.T) {
	for _, change := range []string{"same", "pid", "mode", "uptime", "error"} {
		t.Run(change, func(t *testing.T) {
			baseline := fixtureStatus()
			status := baseline
			var failure error
			switch change {
			case "same":
				status.UptimeSec++
			case "pid":
				status.PID++
			case "mode":
				status.Mode = "presence"
			case "uptime":
				status.UptimeSec--
			case "error":
				failure = errors.New("private-loader-detail")
			}
			model := NewObserved(context.Background(), func(context.Context) (Snapshot, error) { return status, failure }, baseline)
			defer model.Close()
			if !model.haveStatus || model.status != baseline {
				t.Fatal("viewer dropped its validated preflight baseline")
			}
			_, next := model.Update(model.Init()())
			if change == "same" {
				if model.unavailable || model.status != status || next == nil {
					t.Fatal("same observed owner could not continue polling")
				}
			} else if !model.unavailable || model.haveStatus || model.status != (Snapshot{}) || next != nil || strings.Contains(model.View(), "owned-node") {
				t.Fatal("first viewer poll silently adopted a changed owner or kept stale fields")
			}
		})
	}
}

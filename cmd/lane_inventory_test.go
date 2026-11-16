package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/laneinventory"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
	"github.com/spf13/cobra"
)

func TestLaneInventoryNodeHandlerWiring(t *testing.T) {
	const org, node = "fixture-org", "42"
	called := 0
	handlers, _ := buildNodeJobHandlers(nodeJobHandlerOpts{
		OrgID: org, NodeID: node, WorkspaceDir: t.TempDir(), PermissionsDir: t.TempDir(),
		InventoryEnabled: func() bool { return true },
		ShellDisabled:    true, DesktopDisabled: true, FilesDisabled: true,
		HandlerLog: func(string, ...any) {},
		InventoryCollect: func(context.Context) (laneinventory.Report, error) {
			called++
			return laneinventory.Report{Panes: []laneinventory.Pane{}}, nil
		},
	})
	runner := worker.NewRunner(nil, handlers, worker.RunnerConfig{})
	supported := false
	for _, kind := range runner.SupportedJobTypes() {
		if kind == worker.JobTypeLaneInventory {
			supported = true
		}
	}
	if !supported || !runner.CanHandle(worker.JobTypeLaneInventory) {
		t.Fatal("inventory absent from real handler set")
	}
	queue := nodeQueueName(org, node)
	if queue != "jobs:v1:shell:org_fixture-org:node:42" {
		t.Fatal("node queue contract changed")
	}
	for _, h := range handlers {
		if !h.CanHandle(worker.JobTypeLaneInventory) {
			continue
		}
		for _, q := range []string{queue, nodeQueueName(node, org)} {
			before := called
			r, err := h.Execute(context.Background(), &worker.Job{Type: worker.JobTypeLaneInventory, Source: "redis-api", SourceQueue: q}, nil)
			allowed := q == queue
			if err != nil || (r.Status == worker.JobStatusSuccess) != allowed || (called == before+1) != allowed {
				t.Fatalf("routing mismatch: queue=%s result=%+v calls=%d err=%v", q, r, called, err)
			}
		}
		if called != 1 {
			t.Fatal("collector not called exactly once")
		}
		return
	}
	t.Fatal("inventory handler missing")
}

func TestLaneInventoryNodeDefaultGate(t *testing.T) {
	for _, value := range []string{"", "0", "true", "yes", "1", " 1"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("CITADEL_LANE_INVENTORY", value)
			called := 0
			handlers, _ := buildNodeJobHandlers(nodeJobHandlerOpts{
				OrgID: "org", NodeID: "42", WorkspaceDir: t.TempDir(), PermissionsDir: t.TempDir(),
				ShellDisabled: true, DesktopDisabled: true, FilesDisabled: true,
				HandlerLog:       func(string, ...any) {},
				InventoryCollect: func(context.Context) (laneinventory.Report, error) { called++; return laneinventory.Report{}, nil },
			})
			runner := worker.NewRunner(nil, handlers, worker.RunnerConfig{})
			advertised := false
			for _, kind := range runner.SupportedJobTypes() {
				if kind == worker.JobTypeLaneInventory {
					advertised = true
				}
			}
			if advertised != (value == "1") || runner.CanHandle(worker.JobTypeLaneInventory) != (value == "1") {
				t.Fatal("default-off capability gate bypassed")
			}
			for _, h := range handlers {
				inventory, ok := h.(*worker.LaneInventoryHandler)
				if !ok {
					continue
				}
				r, err := inventory.Execute(context.Background(), &worker.Job{Type: worker.JobTypeLaneInventory, Source: "redis-api", SourceQueue: nodeQueueName("org", "42")}, nil)
				if err != nil || (r.Status == worker.JobStatusSuccess) != (value == "1") || (called == 1) != (value == "1") {
					t.Fatal("gate did not precede observation")
				}
				if value != "1" && (r.Output["code"] != "lane_inventory_disabled" || !errors.Is(r.Error, worker.ErrLaneInventoryDisabled)) {
					t.Fatal("missing typed refusal")
				}
				// A disable observed after registration immediately revokes both
				// dispatch and the same list used by heartbeat job_types.
				t.Setenv("CITADEL_LANE_INVENTORY", "0")
				if inventory.CanHandle(worker.JobTypeLaneInventory) {
					t.Fatal("disable not rechecked")
				}
			}
		})
	}
}

func TestLaneInventoryLocalCLI(t *testing.T) {
	nodeDir := t.TempDir()
	called, rootRuns := 0, 0
	parent := &cobra.Command{Use: "sessions", PersistentPreRun: declaredLocalOnly}
	parent.AddCommand(newLaneInventoryCommand(&nodeDir, func(_ context.Context, selected string) (laneinventory.Report, error) {
		called++
		if selected != nodeDir {
			t.Fatal("wrong explicit root")
		}
		return laneinventory.Report{Panes: []laneinventory.Pane{}, Capacity: laneinventory.Capacity{NCPU: 2}}, nil
	}))
	root := &cobra.Command{Use: "fixture", PersistentPreRun: func(*cobra.Command, []string) { rootRuns++ }}
	root.AddCommand(parent)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"sessions", "inventory"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if called != 1 || rootRuns != 0 || !json.Valid(out.Bytes()) || out.Len() > laneinventory.MaxBytes {
		t.Fatalf("calls=%d root=%d output=%q", called, rootRuns, out.String())
	}
	if parent, _, err := newDeclaredSessionsCommand(nil).Find([]string{"inventory"}); err != nil || parent.Name() != "inventory" {
		t.Fatal("production command missing")
	}
}

func TestLaneInventoryCLISanitizesProbeError(t *testing.T) {
	dir := t.TempDir()
	command := newLaneInventoryCommand(&dir, func(context.Context, string) (laneinventory.Report, error) {
		return laneinventory.Report{}, errors.New("planted-sensitive-error")
	})
	command.SetArgs(nil)
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	if err := command.Execute(); !errors.Is(err, laneinventory.ErrUnavailable) {
		t.Fatalf("error %v", err)
	}
	if bytes.Contains(out.Bytes(), []byte("planted")) {
		t.Fatal("probe diagnostic leaked")
	}
}

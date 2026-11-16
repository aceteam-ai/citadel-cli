package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/laneinventory"
	"github.com/spf13/cobra"
)

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

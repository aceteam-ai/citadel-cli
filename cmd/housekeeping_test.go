package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/housekeeping"
	"github.com/spf13/cobra"
)

func TestHousekeepingPlanFromRootIsOfflineAndAdvisory(t *testing.T) {
	prior := nodeConfigDirFn
	nodeConfigDirFn = func() string { t.Fatal("offline plan reached node config"); return "" }
	t.Cleanup(func() { nodeConfigDirFn = prior })
	t.Setenv(envProfile, "")
	now := time.Date(2030, 1, 2, 12, 0, 0, 0, time.UTC)
	command := &cobra.Command{Use: "citadel", PersistentPreRun: rootCmd.PersistentPreRun}
	command.AddCommand(newHousekeepingCommand(func() time.Time { return now }))
	command.SetIn(strings.NewReader(`{"version":1,"targets":[{"id":"fixture-cache","kind":"go_build_cache","observation":{"observed_at":"2030-01-02T12:00:00Z","newest_entry_at":"2029-12-01T00:00:00Z","estimated_bytes":12,"available_bytes":1,"open_handles":"no","in_use":"no"}}]}`))
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"housekeeping", "plan"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var receipt housekeeping.Receipt
	if json.Unmarshal(output.Bytes(), &receipt) != nil || receipt.DeletionEnabled || receipt.FreedBytes != 0 || receipt.EstimatedCandidateBytes != 12 || receipt.Source != "supplied_metadata_only" {
		t.Fatal("command output claims live inspection or deletion")
	}
}

func TestHousekeepingStartupExceptionIsExactAndHPCStaysClosed(t *testing.T) {
	for _, name := range []string{"plan", "status", "run", "future"} {
		root := &cobra.Command{Use: "citadel"}
		parent := &cobra.Command{Use: "housekeeping"}
		leaf := &cobra.Command{Use: name}
		root.AddCommand(parent)
		parent.AddCommand(leaf)
		if isHousekeepingPlanCommand(leaf) != (name == "plan") {
			t.Fatal("broad startup exemption")
		}
		if hpcProfileAllowsCommand(leaf.CommandPath()) {
			t.Fatal("housekeeping expanded HPC admission")
		}
	}
}

type housekeepingFailWriter struct{}

func (housekeepingFailWriter) Write([]byte) (int, error) {
	return 0, errors.New("fixture-private-error")
}

func TestHousekeepingPlanRejectsMalformedInputAndOutputFailure(t *testing.T) {
	for _, raw := range []string{"", "null", `{"version":1,"targets":[],"path":"fixture-private-path"}`} {
		command := newHousekeepingCommand(time.Now)
		var output bytes.Buffer
		command.SetIn(strings.NewReader(raw))
		command.SetOut(&output)
		command.SetErr(&output)
		command.SetArgs([]string{"plan"})
		if err := command.Execute(); err != housekeeping.ErrInput || strings.Contains(output.String(), "fixture-private-path") {
			t.Fatal("malformed input accepted or leaked")
		}
	}
	command := newHousekeepingCommand(time.Now)
	command.SetArgs([]string{"plan"})
	command.SetIn(strings.NewReader(`{"version":1,"targets":[]}`))
	command.SetOut(housekeepingFailWriter{})
	command.SetErr(new(bytes.Buffer))
	if err := command.Execute(); err == nil || err.Error() != "housekeeping_output_failed" {
		t.Fatal("output failure lost or leaked")
	}
}

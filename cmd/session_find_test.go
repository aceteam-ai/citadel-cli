package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/laneinventory"
	"github.com/aceteam-ai/citadel-cli/internal/sessionsearch"
	"github.com/spf13/cobra"
)

func TestSessionFindCommandLocalOnly(t *testing.T) {
	var output bytes.Buffer
	rootCalls, findCalls, inventoryCalls := 0, 0, 0
	root := &cobra.Command{Use: "citadel", PersistentPreRun: func(*cobra.Command, []string) { rootCalls++ }}
	parent := &cobra.Command{Use: "sessions", PersistentPreRun: declaredLocalOnly}
	root.AddCommand(parent)
	dir := "fixture-node"
	parent.AddCommand(newSessionFindCommand(&dir, func(_ context.Context, o sessionsearch.Options, _ []laneinventory.Pane) (sessionsearch.Report, error) {
		findCalls++
		if o.Query != "violet compass" || o.Harness != "codex" || o.Since.Year() != 2026 {
			t.Fatal("flags not passed")
		}
		return sessionsearch.Report{Hits: []sessionsearch.Hit{{ID: "11111111-2222-4333-8444-555555555555", Harness: "codex"}}}, nil
	}, func(context.Context, string) (laneinventory.Report, error) {
		inventoryCalls++
		return laneinventory.Report{}, errors.New("private error")
	}))
	root.SetOut(&output)
	root.SetArgs([]string{"sessions", "find", "violet", "compass", "--harness", "codex", "--since", "2026-11-01", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if rootCalls != 0 || findCalls != 1 || inventoryCalls != 1 || strings.Contains(output.String(), "private error") || !strings.Contains(output.String(), `"location"`) {
		t.Fatal("unexpected command output or startup")
	}
	if leaf, _, err := newDeclaredSessionsCommand(nil).Find([]string{"find"}); err != nil || leaf.Name() != "find" {
		t.Fatal("real sessions command not wired")
	}
}
func TestSessionFindCommandErrorWithholding(t *testing.T) {
	dir := "fixture-node"
	c := newSessionFindCommand(&dir, func(context.Context, sessionsearch.Options, []laneinventory.Pane) (sessionsearch.Report, error) {
		return sessionsearch.Report{}, errors.New("private source path")
	}, nil)
	c.SetArgs([]string{"needle"})
	if err := c.Execute(); !errors.Is(err, sessionsearch.ErrUnavailable) {
		t.Fatal("raw source error escaped")
	}
}

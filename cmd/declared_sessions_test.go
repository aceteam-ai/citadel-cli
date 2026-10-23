package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/declaredsession"
	"github.com/spf13/cobra"
)

// These fixtures use only an explicit directory and fake lifecycle. Executing
// them must not inspect real node config, systemd, sockets, PTYs or credentials.
type declaredFakeLifecycle struct {
	action string
	name   string
	err    error
}

func (f *declaredFakeLifecycle) result(action, name string) (declaredsession.ManagedStatus, error) {
	f.action, f.name = action, name
	return declaredsession.ManagedStatus{Name: name, Classification: "unconfirmed"}, f.err
}

func (f *declaredFakeLifecycle) Inventory(context.Context) ([]declaredsession.ManagedStatus, error) {
	result, err := f.result("list", "fixture")
	return []declaredsession.ManagedStatus{result}, err
}

func (f *declaredFakeLifecycle) Status(_ context.Context, name string) (declaredsession.ManagedStatus, error) {
	return f.result("status", name)
}

func (f *declaredFakeLifecycle) Start(_ context.Context, name string) (declaredsession.ManagedStatus, error) {
	return f.result("start", name)
}

func (f *declaredFakeLifecycle) Stop(_ context.Context, name string) (declaredsession.ManagedStatus, error) {
	return f.result("stop", name)
}

func (f *declaredFakeLifecycle) Restart(_ context.Context, name string) (declaredsession.ManagedStatus, error) {
	return f.result("restart", name)
}

func TestDeclaredLifecycleLocalOnly(t *testing.T) {
	for _, action := range []string{"list", "status", "start", "stop", "restart"} {
		t.Run(action, func(t *testing.T) {
			dir := declaredCommandTempDir(t)
			fake := &declaredFakeLifecycle{}
			opened, closed, rootRuns := 0, 0, 0
			root := &cobra.Command{Use: "fixture", PersistentPreRun: func(*cobra.Command, []string) { rootRuns++ }}
			root.AddCommand(newDeclaredSessionsCommand(func(selected string) (declaredLifecycle, func() error, error) {
				opened++
				if selected != dir {
					t.Fatalf("selected unexpected directory: %q", selected)
				}
				return fake, func() error { closed++; return nil }, nil
			}))
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			args := []string{"sessions", "--node-config-dir", dir, action}
			if action != "list" {
				args = append(args, "fixture")
			}
			root.SetArgs(args)
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if opened != 1 || closed != 1 || rootRuns != 0 || fake.action != action {
				t.Fatalf("opened=%d closed=%d root=%d action=%q", opened, closed, rootRuns, fake.action)
			}
			if !json.Valid(output.Bytes()) {
				t.Fatalf("invalid status JSON: %q", output.String())
			}
		})
	}
}

func TestDeclaredStartupHooksPreserveLocalIsolationAndLegacyAttach(t *testing.T) {
	original := rootCmd.PersistentPreRun
	t.Cleanup(func() { rootCmd.PersistentPreRun = original })
	calls := 0
	rootCmd.PersistentPreRun = func(command *cobra.Command, args []string) {
		calls++
		if command != attachCmd || len(args) != 0 {
			t.Fatal("unexpected delegation to root startup")
		}
	}
	for _, command := range []*cobra.Command{
		newDeclaredSessionsCommand(nil), newDeclaredHostCommand(),
	} {
		if command.PersistentPreRun == nil || command.PersistentPreRunE != nil {
			t.Fatal("local-only startup hook missing or shadowed")
		}
		command.PersistentPreRun(command, []string{"fixture"})
	}
	attachCmd.PersistentPreRun(attachCmd, []string{"fixture"})
	if calls != 0 {
		t.Fatal("local declared-session startup invoked root")
	}
	attachCmd.PersistentPreRun(attachCmd, nil)
	if calls != 1 {
		t.Fatal("legacy no-name attach did not delegate to root exactly once")
	}
}

func TestDeclaredFailureClassificationAndClose(t *testing.T) {
	want := errors.New("fixture lifecycle refused")
	fake := &declaredFakeLifecycle{err: want}
	closed := 0
	command := newDeclaredSessionsCommand(func(string) (declaredLifecycle, func() error, error) {
		return fake, func() error { closed++; return nil }, nil
	})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(new(bytes.Buffer))
	command.SetArgs([]string{"--node-config-dir", declaredCommandTempDir(t), "status", "fixture"})
	err := command.Execute()
	if !errors.Is(err, want) || closed != 1 || !strings.Contains(output.String(), `"classification":"unconfirmed"`) {
		t.Fatalf("err=%v closed=%d output=%q", err, closed, output.String())
	}
}

func TestDeclaredBadNameHasNoStorageEffect(t *testing.T) {
	opened := false
	command := newDeclaredSessionsCommand(func(string) (declaredLifecycle, func() error, error) {
		opened = true
		return nil, nil, errors.New("unexpected storage access")
	})
	command.SetErr(new(bytes.Buffer))
	command.SetArgs([]string{"--node-config-dir", declaredCommandTempDir(t), "start", "../escape"})
	if err := command.Execute(); err == nil || opened {
		t.Fatalf("err=%v opened=%v", err, opened)
	}
}

func declaredAttachArgsFixture() *cobra.Command {
	command := &cobra.Command{}
	command.Flags().Bool("readonly", false, "")
	command.Flags().Bool("take-control", false, "")
	command.Flags().String("detach-keys", defaultDeclaredDetach, "")
	command.Flags().String("node-config-dir", "", "")
	return command
}

func TestDeclaredAttachArguments(t *testing.T) {
	for _, test := range []struct {
		name    string
		args    []string
		flags   []string
		wantErr bool
	}{
		{name: "legacy"},
		{name: "named", args: []string{"fixture"}},
		{name: "too many", args: []string{"fixture", "other"}, wantErr: true},
		{name: "bad name", args: []string{"Fixture"}, wantErr: true},
		{name: "no implicit named readonly", flags: []string{"--readonly"}, wantErr: true},
		{name: "no implicit named directory", flags: []string{"--node-config-dir=/tmp/fixture"}, wantErr: true},
		{name: "role conflict", args: []string{"fixture"}, flags: []string{"--readonly", "--take-control"}, wantErr: true},
		{name: "invalid chord", args: []string{"fixture"}, flags: []string{"--detach-keys=no"}, wantErr: true},
		{name: "readonly", args: []string{"fixture"}, flags: []string{"--readonly"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := declaredAttachArgsFixture()
			if err := command.Flags().Parse(test.flags); err != nil {
				t.Fatal(err)
			}
			if err := validateDeclaredAttach(command, test.args); (err != nil) != test.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, test.wantErr)
			}
		})
	}
}

func TestDeclaredNodeOverrideRefusesBeforeLifecycleOrLogStorage(t *testing.T) {
	original := nodeDirFlag
	t.Cleanup(func() { nodeDirFlag = original })
	for _, test := range []struct {
		name string
		flag string
		env  string
	}{
		{name: "environment", env: "/not-this-node"},
		{name: "flag", flag: "/not-this-node"},
		{name: "explicit empty flag", flag: ""},
	} {
		for _, action := range []string{"list", "status", "start", "stop", "restart", "logs"} {
			t.Run(test.name+"/"+action, func(t *testing.T) {
				nodeDirFlag = ""
				t.Setenv("CITADEL_NODE_DIR", test.env)
				opened := false
				root := &cobra.Command{Use: "fixture"}
				root.PersistentFlags().StringVar(&nodeDirFlag, "node-dir", "", "")
				root.AddCommand(newDeclaredSessionsCommand(func(string) (declaredLifecycle, func() error, error) {
					opened = true
					return nil, nil, errors.New("unexpected lifecycle storage")
				}))
				root.SetErr(new(bytes.Buffer))
				args := []string{"sessions", "--node-config-dir", "/never-open-this", action}
				if action != "list" {
					args = append(args, "fixture")
				}
				if test.name != "environment" {
					args = append(args, "--node-dir="+test.flag)
				}
				root.SetArgs(args)
				err := root.Execute()
				if err == nil || !strings.Contains(err.Error(), "CITADEL_NODE_DIR") || opened {
					t.Fatalf("override reached storage: err=%v opened=%v", err, opened)
				}
			})
		}
	}
}

func TestDeclaredHostAndNamedAttachRefuseNodeOverridesBeforeNativeWork(t *testing.T) {
	original := nodeDirFlag
	t.Cleanup(func() { nodeDirFlag = original })
	nodeDirFlag = ""
	t.Setenv("CITADEL_NODE_DIR", "/not-this-node")
	called := false
	host := newDeclaredHostCommandWith(func(context.Context, declaredsession.RunHolderOptions) error {
		called = true
		return errors.New("unexpected fake holder launch")
	})
	if err := host.Flags().Set("node-config-dir", declaredCommandTempDir(t)); err != nil {
		t.Fatal(err)
	}
	if err := host.RunE(host, []string{"fixture"}); err == nil || !strings.Contains(err.Error(), "CITADEL_NODE_DIR") || called {
		t.Fatalf("host failed to reject override before native launch: err=%v called=%v", err, called)
	}
	command := declaredAttachArgsFixture()
	if err := validateDeclaredAttach(command, []string{"fixture"}); err == nil || !strings.Contains(err.Error(), "CITADEL_NODE_DIR") {
		t.Fatalf("named attach failed to reject override: %v", err)
	}
	if err := validateDeclaredAttach(command, nil); err != nil {
		t.Fatalf("legacy no-name attach behavior changed: %v", err)
	}
}

func TestDeclaredExplicitNodeConfigDirectorySelectedWithoutRealNodeLookup(t *testing.T) {
	original := nodeDirFlag
	t.Cleanup(func() { nodeDirFlag = original })
	nodeDirFlag = ""
	t.Setenv("CITADEL_NODE_DIR", "")
	dir := declaredCommandTempDir(t)
	got, err := selectedDeclaredNodeDir(&cobra.Command{}, dir)
	if err != nil || got != dir {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func declaredCommandTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

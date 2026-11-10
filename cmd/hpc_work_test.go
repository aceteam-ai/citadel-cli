package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/hpc"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
	"github.com/spf13/pflag"
)

const testHPCNodeID = "hpc-0123456789abcdef0123456789abcdef"

func TestResolveProfileSelection(t *testing.T) {
	cases := []struct {
		flag    string
		flagSet bool
		env     string
		want    string
		wantErr string
	}{
		{want: ""},
		{env: "hpc", want: "hpc"},
		{env: " HPC ", want: "hpc"},
		{flag: "hpc", flagSet: true, want: "hpc"},
		{flag: "hpc", flagSet: true, env: "hpc", want: "hpc"},
		{flag: "default", flagSet: true, want: ""},
		{flag: "hpc", flagSet: true, env: "default", wantErr: "disagree"},
		{flag: "", flagSet: true, env: "hpc", wantErr: "disagree"},
		{env: "hcp", wantErr: "unknown profile"},
		{flag: "slurm", flagSet: true, wantErr: "unknown profile"},
	}
	for _, tc := range cases {
		got, err := resolveProfileSelection(tc.flag, tc.flagSet, tc.env)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("(%q,%v,%q) err = %v, want %q", tc.flag, tc.flagSet, tc.env, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("(%q,%v,%q) = %q, %v; want %q", tc.flag, tc.flagSet, tc.env, got, err, tc.want)
		}
	}
}

func TestHPCProfileAllowsOnlyListedCommands(t *testing.T) {
	for _, path := range []string{"citadel work", "citadel version", "citadel hpc", "citadel hpc enroll"} {
		if !hpcProfileAllowsCommand(path) {
			t.Errorf("%q should be allowed", path)
		}
	}
	for _, path := range []string{"citadel", "citadel init", "citadel login", "citadel local mcp", "citadel mcp",
		"citadel up", "citadel run", "citadel update install", "citadel service install", "citadel hpcx", "citadel status"} {
		if hpcProfileAllowsCommand(path) {
			t.Errorf("%q must be refused", path)
		}
	}
}

func TestResolveHPCActivation(t *testing.T) {
	base := t.TempDir()
	env := map[string]string{hpc.EnvJobID: "77", hpc.EnvTmpDir: base}
	getenv := func(k string) string { return env[k] }
	uid := os.Getuid()
	if uid == 0 {
		t.Skip("activation refuses root")
	}
	layout, err := resolveHPCActivation("citadel work", "linux", uid, "", getenv)
	if err != nil {
		t.Fatalf("activation: %v", err)
	}
	if layout.Root != filepath.Join(base, "citadel-job-77") {
		t.Fatalf("root = %s", layout.Root)
	}
	if _, err := os.Stat(layout.NodeDir); err != nil {
		t.Fatalf("node dir not prepared: %v", err)
	}
	explicit := t.TempDir()
	layout, err = resolveHPCActivation("citadel work", "linux", uid, explicit, getenv)
	if err != nil || layout.Base != explicit {
		t.Fatalf("explicit root: %v, %s", err, layout.Base)
	}
	if _, err := resolveHPCActivation("citadel init", "linux", uid, "", getenv); err == nil {
		t.Fatal("init must be refused under the hpc profile")
	}
	if _, err := resolveHPCActivation("citadel work", "linux", 0, "", getenv); err == nil {
		t.Fatal("root must be refused")
	}
	if _, err := resolveHPCActivation("citadel work", "windows", uid, "", getenv); !errors.Is(err, hpc.ErrUnsupportedPlatform) {
		t.Fatalf("windows: %v", err)
	}
	if _, err := resolveHPCActivation("citadel work", "linux", uid, "", func(string) string { return "" }); err == nil {
		t.Fatal("a missing SLURM_JOB_ID must be refused")
	}
}

func TestHPCCredentialsFrom(t *testing.T) {
	good := &DeviceConfig{DeviceAPIToken: "tok", OrgID: "org-1", APIBaseURL: "https://example.test", FabricNodeID: testHPCNodeID}
	c, err := hpcCredentialsFrom(good)
	if err != nil || c.NodeID != testHPCNodeID || c.OrgID != "org-1" || c.Token != "tok" {
		t.Fatalf("got %+v, %v", c, err)
	}
	for name, dc := range map[string]*DeviceConfig{
		"nil":          nil,
		"no token":     {OrgID: "o", APIBaseURL: "https://x", FabricNodeID: testHPCNodeID},
		"numeric node": {DeviceAPIToken: "t", OrgID: "o", APIBaseURL: "https://x", FabricNodeID: "758"},
		"no node":      {DeviceAPIToken: "t", OrgID: "o", APIBaseURL: "https://x"},
		"no org":       {DeviceAPIToken: "t", APIBaseURL: "https://x", FabricNodeID: testHPCNodeID},
		"no base":      {DeviceAPIToken: "t", OrgID: "o", FabricNodeID: testHPCNodeID},
	} {
		if _, err := hpcCredentialsFrom(dc); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestHPCQueueNamesAreOnlyThePerNodeStream(t *testing.T) {
	got := hpcQueueNames("org-1", testHPCNodeID)
	want := []string{"jobs:v1:shell:org_org-1:node:" + testHPCNodeID}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("queues = %v, want %v", got, want)
	}
}

func TestHPCRefusedWorkFlags(t *testing.T) {
	fs := pflag.NewFlagSet("work", pflag.ContinueOnError)
	for _, name := range []string{"poll-ms", "max-retries", "max-concurrency", "gateway", "terminal", "status-port", "debug-redis-url", "egress-relay", "queue", "workspace"} {
		fs.String(name, "", "")
	}
	if got := hpcRefusedWorkFlags(fs); len(got) != 0 {
		t.Fatalf("no flags changed, got %v", got)
	}
	_ = fs.Set("max-concurrency", "2")
	_ = fs.Set("poll-ms", "1000")
	if got := hpcRefusedWorkFlags(fs); len(got) != 0 {
		t.Fatalf("allowed flags refused: %v", got)
	}
	_ = fs.Set("gateway", "true")
	_ = fs.Set("status-port", "8080")
	_ = fs.Set("queue", "jobs:v1:gpu-general")
	got := hpcRefusedWorkFlags(fs)
	want := []string{"--gateway", "--queue", "--status-port"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("refused = %v, want %v", got, want)
	}
	// Every real `citadel work` flag outside the allowlist is refused.
	workCmd.Flags().VisitAll(func(f *pflag.Flag) {
		if hpcWorkFlagsAllowed[f.Name] {
			return
		}
		probe := pflag.NewFlagSet("probe", pflag.ContinueOnError)
		probe.String(f.Name, "", "")
		_ = probe.Set(f.Name, "x")
		if len(hpcRefusedWorkFlags(probe)) != 1 {
			t.Errorf("--%s is not refused", f.Name)
		}
	})
}

func TestHPCCapabilityPolicyOptInsFailClosed(t *testing.T) {
	if err := (hpcCapabilityPolicy{}).validate(); err != nil {
		t.Fatalf("default policy: %v", err)
	}
	for _, p := range []hpcCapabilityPolicy{{allowShell: true}, {allowFiles: true}} {
		if _, err := buildHPCJobHandlers(hpcHandlerOpts{Policy: p}); err == nil {
			t.Fatalf("%+v must refuse until confinement exists", p)
		}
	}
}

func TestHPCTemplateBuiltinPolicy(t *testing.T) {
	if r := hpcTemplateBuiltinPolicy(audioMixBuiltinName); r != "" {
		t.Fatalf("audio-mix refused: %s", r)
	}
	for _, name := range []string{papercraftBuiltinName, papercraftSceneBuiltinName, papercraftSceneV2BuiltinName} {
		if r := hpcTemplateBuiltinPolicy(name); !strings.Contains(r, "browser debug listener") {
			t.Fatalf("%s: reason %q", name, r)
		}
	}
	if hpcTemplateBuiltinPolicy("something-new") == "" {
		t.Fatal("an unlisted builtin must be refused by default")
	}
}

func TestHPCTemplateRunOpsRefusesBeforeRunningBuiltin(t *testing.T) {
	invoked := false
	registerTestBuiltin(t, "test-hpc-unlisted", func(context.Context, json.RawMessage, []string, string) ([]string, error) {
		invoked = true
		return nil, nil
	})
	ops := hpcTemplateRunOps(t.TempDir(), "n1")
	for _, handler := range []string{papercraftBuiltinName, "test-hpc-unlisted"} {
		_, err := ops.Run(context.Background(), verifiedTemplateRequest(t, worker.TemplateRunRequest{
			JobID:       "job-hpc",
			TemplateKey: "t",
			Runner:      json.RawMessage(`{"kind":"builtin","handler":"` + handler + `"}`),
		}))
		if !errors.Is(err, errTemplateBuiltinRefused) {
			t.Fatalf("%s: err = %v, want errTemplateBuiltinRefused", handler, err)
		}
	}
	if invoked {
		t.Fatal("a refused builtin ran")
	}
}

// hpcFakeSource serves a fixed list of jobs, then idles until ctx ends.
type hpcFakeSource struct {
	mu     sync.Mutex
	jobs   []*worker.Job
	failed map[string]error
	acked  map[string]bool
	done   chan struct{}
}

func newHPCFakeSource(jobs ...*worker.Job) *hpcFakeSource {
	return &hpcFakeSource{jobs: jobs, failed: map[string]error{}, acked: map[string]bool{}, done: make(chan struct{}, len(jobs)+1)}
}

func (s *hpcFakeSource) Name() string                  { return "hpc-fake" }
func (s *hpcFakeSource) Connect(context.Context) error { return nil }
func (s *hpcFakeSource) Next(ctx context.Context) (*worker.Job, error) {
	s.mu.Lock()
	if len(s.jobs) > 0 {
		j := s.jobs[0]
		s.jobs = s.jobs[1:]
		s.mu.Unlock()
		return j, nil
	}
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(10 * time.Millisecond):
		return nil, nil
	}
}
func (s *hpcFakeSource) Ack(_ context.Context, j *worker.Job) error {
	s.mu.Lock()
	s.acked[j.ID] = true
	s.mu.Unlock()
	s.done <- struct{}{}
	return nil
}
func (s *hpcFakeSource) Nack(_ context.Context, j *worker.Job, err error) error {
	return s.Fail(context.Background(), j, err, nil)
}
func (s *hpcFakeSource) Fail(_ context.Context, j *worker.Job, err error, _ map[string]any) error {
	s.mu.Lock()
	s.failed[j.ID] = err
	s.mu.Unlock()
	s.done <- struct{}{}
	return nil
}
func (s *hpcFakeSource) IsJobCancelled(context.Context, string) bool { return false }
func (s *hpcFakeSource) Close() error                                { return nil }

func testHPCDeps(t *testing.T, src worker.JobSource) hpcWorkerDeps {
	t.Helper()
	root := t.TempDir()
	return hpcWorkerDeps{
		Creds:  hpcCredentials{NodeID: testHPCNodeID, OrgID: "org-1", Token: "tok", APIBaseURL: "https://example.test"},
		Layout: hpc.Layout{Root: root, WorkspaceDir: filepath.Join(root, "workspace"), NodeDir: filepath.Join(root, "node")},
		Source: src,
		State:  worker.NewWorkerState(),
	}
}

// TestHPCHandlerSetIsInferenceAndTemplatesOnly pins the complete dispatchable
// set, and that a permission file granting everything cannot widen it: the
// HPC worker never reads permissions.yaml for its handler set.
func TestHPCHandlerSetIsInferenceAndTemplatesOnly(t *testing.T) {
	nodeDir := network.GetNodeConfigDir() // the cmd test fixture dir
	permPath := filepath.Join(nodeDir, "permissions.yaml")
	if _, err := os.Stat(permPath); err == nil {
		t.Skip("fixture already holds a permissions file")
	}
	t.Cleanup(func() { _ = os.Remove(permPath) })

	want := []string{"RUN_JOB_TEMPLATE", "llm_inference"}
	check := func(stage string) {
		runner, err := newHPCRunner(testHPCDeps(t, newHPCFakeSource()))
		if err != nil {
			t.Fatal(err)
		}
		got := runner.SupportedJobTypes()
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: job types = %v, want %v", stage, got, want)
		}
		if runner.NodeID() != testHPCNodeID {
			t.Fatalf("%s: runner node id = %q", stage, runner.NodeID())
		}
	}
	check("default")
	all := &config.Permissions{Shell: true, Files: true, Desktop: true, Console: true, Services: true, SSH: true}
	if err := config.SavePermissions(nodeDir, all); err != nil {
		t.Fatal(err)
	}
	check("after a permission refresh granting everything")
	if p := hpcPermissions(); p.Shell || p.Files || p.Desktop || p.Console || p.Services || p.SSH {
		t.Fatalf("reported permissions widened: %+v", p)
	}
}

// TestHPCWorkerRefusesShellAfterPermissionRefresh drives real dispatch: a
// SHELL_COMMAND on the node's own stream fails terminally even with a
// permission file that grants shell.
func TestHPCWorkerRefusesShellAfterPermissionRefresh(t *testing.T) {
	nodeDir := network.GetNodeConfigDir()
	permPath := filepath.Join(nodeDir, "permissions.yaml")
	if _, err := os.Stat(permPath); err == nil {
		t.Skip("fixture already holds a permissions file")
	}
	t.Cleanup(func() { _ = os.Remove(permPath) })
	if err := config.SavePermissions(nodeDir, &config.Permissions{Shell: true, Files: true}); err != nil {
		t.Fatal(err)
	}
	queue := nodeQueueName("org-1", testHPCNodeID)
	jobs := []*worker.Job{
		{ID: "shell-1", Type: "SHELL_COMMAND", Payload: map[string]any{"command": "id"}, SourceQueue: queue, MessageID: "1-0"},
		{ID: "files-1", Type: "FILE_READ", Payload: map[string]any{"path": "/etc/hostname"}, SourceQueue: queue, MessageID: "2-0"},
	}
	src := newHPCFakeSource(jobs...)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- runHPCWorker(ctx, testHPCDeps(t, src)) }()
	for range jobs {
		select {
		case <-src.done:
		case <-ctx.Done():
			t.Fatal("jobs were not settled")
		}
	}
	cancel()
	<-errCh
	src.mu.Lock()
	defer src.mu.Unlock()
	for _, j := range jobs {
		if src.acked[j.ID] {
			t.Fatalf("%s was acknowledged as a success", j.ID)
		}
		if src.failed[j.ID] == nil {
			t.Fatalf("%s was not failed", j.ID)
		}
	}
}

func TestHPCProfileFilesAreUnixOnlyAtRuntime(t *testing.T) {
	if runtime.GOOS == "windows" {
		if _, err := resolveHPCActivation("citadel work", runtime.GOOS, 1000, "", os.Getenv); !errors.Is(err, hpc.ErrUnsupportedPlatform) {
			t.Fatalf("windows activation: %v", err)
		}
	}
}

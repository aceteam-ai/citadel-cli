package cmd

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/composerefresh"
	"github.com/aceteam-ai/citadel-cli/services"
)

// These tests drive the template-refresh recreate through managedCompose's
// seams: no container engine is executed and no real node directory is read.

// prePinTEICompose rebuilds the TEI template shipped before the model revision
// pin and checks it is the registered historical hash.
func prePinTEICompose(t *testing.T) string {
	t.Helper()
	old := strings.Replace(services.ServiceMap["tei"], `    # The model revision is pinned. Upstream's 2026-11-21 config change lists a
    # token-classification architecture without id2label, which TEI 1.6 refuses
    # to load, so an unpinned "main" crash-loops on every fresh start.
`, "", 1)
	old = strings.Replace(old, `, "--revision", "9bbca17d9273fd0d03d5725c7a4b0f6b45142062"`, "", 1)
	if !services.KnownComposeHashes["tei"][fmt.Sprintf("%x", sha256.Sum256([]byte(old)))] {
		t.Fatal("pre-pin TEI template is not a registered historical hash")
	}
	return old
}

type fakeManagedCompose struct {
	engine   string
	state    map[string]string // container -> State.Status; absent = no container
	ports    map[string]int
	runs     [][]string
	runErr   error
	stateHit int
}

func installFakeManagedCompose(t *testing.T, f *fakeManagedCompose) {
	t.Helper()
	t.Setenv("CITADEL_NODE_DIR", "")
	t.Setenv("CITADEL_WORKSPACE", t.TempDir())
	t.Setenv("CITADEL_COMPOSE_NO_RECREATE_ON_UPGRADE", "")
	prev := managedCompose
	t.Cleanup(func() { managedCompose = prev })
	if f.engine == "" {
		f.engine = "docker"
	}
	managedCompose = managedComposeDeps{
		runtime: func() catalog.ContainerRuntime {
			return catalog.ContainerRuntime{EngineBin: f.engine, Bin: f.engine, ComposePrefix: []string{"compose"}}
		},
		containerState: func(_, name string) (string, bool) {
			f.stateHit++
			s, ok := f.state[name]
			return s, ok
		},
		publishedPort: func(_, name string) (int, bool) {
			p, ok := f.ports[name]
			return p, ok
		},
		serviceFor: manifestServiceAt,
		run: func(cmd *exec.Cmd) ([]byte, error) {
			f.runs = append(f.runs, append([]string(nil), cmd.Args...))
			return []byte("compose output"), f.runErr
		},
	}
}

// writeNodeFixture lays out a canonical fixture node (sandboxed HOME, global
// pointer, citadel.yaml) with services/<name>.yml, and returns the node dir and
// compose path.
func writeNodeFixture(t *testing.T, name, compose, desired string) (string, string) {
	t.Helper()
	dir := writeManifestWithServices(t, []Service{{Name: name, ComposeFile: "services/" + name + ".yml", DesiredStatus: desired}})
	path := filepath.Join(dir, "services", name+".yml")
	if err := os.WriteFile(path, []byte(compose), 0600); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestManagedComposeRecreatorAppliesRefreshedFileToRunningContainerOnly(t *testing.T) {
	cases := []struct {
		name      string
		engine    string
		desired   string
		state     map[string]string
		wantRuns  int
		wantForce bool
	}{
		{name: "running docker", state: map[string]string{"citadel-tei": "running"}, wantRuns: 1},
		{name: "running podman forces", engine: "podman", state: map[string]string{"citadel-tei": "running"}, wantRuns: 1, wantForce: true},
		{name: "exited", state: map[string]string{"citadel-tei": "exited"}},
		{name: "absent", state: map[string]string{}},
		{name: "operator stopped", desired: "stopped", state: map[string]string{"citadel-tei": "running"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeManagedCompose{engine: tc.engine, state: tc.state}
			installFakeManagedCompose(t, f)
			_, path := writeNodeFixture(t, "tei", services.ServiceMap["tei"], tc.desired)
			recreated, err := managedComposeRecreatorUnchecked("tei", path, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(f.runs) != tc.wantRuns || recreated != (tc.wantRuns == 1) {
				t.Fatalf("runs = %v, recreated = %v; want %d run(s)", f.runs, recreated, tc.wantRuns)
			}
			if tc.desired == "stopped" && f.stateHit != 0 {
				t.Fatal("stopped service was inspected")
			}
			if tc.wantRuns == 0 {
				return
			}
			args := f.runs[0]
			if !hasArg(args, path) || !hasArg(args, "up") || !hasArg(args, "-d") || hasArg(args, "--force-recreate") != tc.wantForce {
				t.Fatalf("compose argv = %v, want up -d from %s (force=%v)", args, path, tc.wantForce)
			}
		})
	}
}

func TestManagedComposeRecreatorForcesWhenHostPortMoved(t *testing.T) {
	for _, tc := range []struct {
		port      int
		wantForce bool
	}{{port: 8080, wantForce: true}, {port: 8200}} {
		f := &fakeManagedCompose{state: map[string]string{"citadel-llamacpp": "running"}, ports: map[string]int{"citadel-llamacpp": tc.port}}
		installFakeManagedCompose(t, f)
		_, path := writeNodeFixture(t, "llamacpp", services.ServiceMap["llamacpp"], "")
		if _, err := managedComposeRecreatorUnchecked("llamacpp", path, 8200); err != nil {
			t.Fatal(err)
		}
		if len(f.runs) != 1 || hasArg(f.runs[0], "--force-recreate") != tc.wantForce {
			t.Fatalf("port %d: runs = %v, want force=%v", tc.port, f.runs, tc.wantForce)
		}
	}
}

// The boot sweep applies a refreshed TEI template to the running container
// exactly once: the same binary's next boot skips the sweep, an operator-edited
// file is preserved and not recreated, and the opt-out leaves it alone.
func TestBootRefreshRecreatesRunningTEIOnceAndNeverInALoop(t *testing.T) {
	old := prePinTEICompose(t)
	for _, tc := range []struct {
		name     string
		compose  string
		optOut   bool
		wantRuns int
	}{
		{name: "pre-pin template", compose: old, wantRuns: 1},
		{name: "operator edited", compose: old + "\n# operator change\n"},
		{name: "opted out", compose: old, optOut: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeManagedCompose{state: map[string]string{"citadel-tei": "running"}}
			installFakeManagedCompose(t, f)
			if tc.optOut {
				t.Setenv("CITADEL_COMPOSE_NO_RECREATE_ON_UPGRADE", "1")
			}
			dir, path := writeNodeFixture(t, "tei", tc.compose, "")
			for i := 0; i < 2; i++ {
				refreshManagedComposeFilesWithSweep(dir, composerefresh.Sweep, func() int { return 0 })
			}
			if len(f.runs) != tc.wantRuns {
				t.Fatalf("compose ups = %v, want %d", f.runs, tc.wantRuns)
			}
			got, _ := os.ReadFile(path)
			if edited := tc.compose != old; edited != (string(got) == tc.compose) {
				t.Fatalf("file after sweep (edited=%v):\n%s", edited, got)
			}
		})
	}
}

// A failed recreate restores the previous file so the next start retries.
func TestBootRefreshRecreateFailureRestoresPreviousTEIFile(t *testing.T) {
	old := prePinTEICompose(t)
	f := &fakeManagedCompose{state: map[string]string{"citadel-tei": "running"}, runErr: errors.New("boom")}
	installFakeManagedCompose(t, f)
	dir, path := writeNodeFixture(t, "tei", old, "")
	refreshManagedComposeFilesWithSweep(dir, composerefresh.Sweep, func() int { return 0 })
	if got, _ := os.ReadFile(path); string(got) != old || len(f.runs) != 1 {
		t.Fatalf("runs = %d; file not rolled back:\n%s", len(f.runs), got)
	}
}

func TestStartReconcileRefreshesKnownTEITemplateOnce(t *testing.T) {
	old := prePinTEICompose(t)
	t.Setenv("CITADEL_NODE_DIR", "")
	_, path := writeNodeFixture(t, "tei", old, "")
	refresh, err := reconcileHostComposeForStartOn("linux", "tei", path, func() int { t.Fatal("TEI probed GPU"); return 0 })
	if err != nil || !refresh.Changed {
		t.Fatalf("first start = %+v, %v; want a refresh", refresh, err)
	}
	if got, _ := os.ReadFile(path); string(got) != services.ServiceMap["tei"] {
		t.Fatal("start did not install the pinned template")
	}
	if again, err := reconcileHostComposeForStartOn("linux", "tei", path, nil); err != nil || again.Changed {
		t.Fatalf("second start = %+v, %v; want no change", again, err)
	}

	edited := old + "\n# operator change\n"
	_, editedPath := writeNodeFixture(t, "tei", edited, "")
	if r, err := reconcileHostComposeForStartOn("linux", "tei", editedPath, nil); err != nil || r.Changed {
		t.Fatalf("edited start = %+v, %v; want untouched", r, err)
	}
	if got, _ := os.ReadFile(editedPath); string(got) != edited {
		t.Fatal("operator edit lost")
	}

	custom := filepath.Join(t.TempDir(), "tei-custom.yml")
	if err := os.WriteFile(custom, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	if r, err := reconcileHostComposeForStartOn("linux", "tei", custom, nil); err != nil || r.Changed {
		t.Fatalf("custom path = %+v, %v; want untouched", r, err)
	}
}

func TestStartReconcileRefreshesNamespacedTEITemplateUnderOverride(t *testing.T) {
	old := prePinTEICompose(t)
	dir := t.TempDir()
	t.Setenv("CITADEL_NODE_DIR", dir)
	namespaced := strings.Replace(old, "container_name: citadel-tei\n", "container_name: "+embeddedContainerName("tei")+"\n", 1)
	if namespaced == old {
		t.Fatal("fixture was not namespaced")
	}
	path := filepath.Join(dir, "services", "tei.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(namespaced), 0600); err != nil {
		t.Fatal(err)
	}
	refresh, err := reconcileHostComposeForStartOn("linux", "tei", path, nil)
	if err != nil || !refresh.Changed {
		t.Fatalf("override start = %+v, %v; want a refresh", refresh, err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "--revision") || !strings.Contains(string(got), "container_name: "+embeddedContainerName("tei")+"\n") {
		t.Fatalf("override refresh lost the namespace or the pin:\n%s", got)
	}
}

func TestRunningStartAction(t *testing.T) {
	for _, tc := range []struct {
		drift, changed, enabled, stopped bool
		want                             runningStartKind
	}{
		{want: runningKeep},
		{enabled: true, want: runningKeep},
		{changed: true, enabled: true, want: runningApplyRefreshedTemplate},
		{changed: true, enabled: false, want: runningKeep},
		{changed: true, enabled: true, stopped: true, want: runningKeep},
		{drift: true, changed: true, enabled: true, want: runningRecreateForBindDrift},
	} {
		if got := runningStartAction(tc.drift, tc.changed, tc.enabled, tc.stopped); got != tc.want {
			t.Fatalf("runningStartAction(%+v) = %v, want %v", tc, got, tc.want)
		}
	}
}

func TestRestartRecreatesOnlyKnownManagedTemplates(t *testing.T) {
	old := prePinTEICompose(t)
	for _, tc := range []struct {
		name        string
		compose     string
		state       map[string]string
		override    bool
		runErr      error
		wantHandled bool
		wantFile    string
	}{
		{name: "pre-pin running", compose: old, state: map[string]string{"citadel-tei": "running"}, wantHandled: true, wantFile: services.ServiceMap["tei"]},
		{name: "current exited", compose: services.ServiceMap["tei"], state: map[string]string{"citadel-tei": "exited"}, wantHandled: true, wantFile: services.ServiceMap["tei"]},
		{name: "operator edited", compose: old + "\n# edit\n", state: map[string]string{"citadel-tei": "running"}, wantFile: old + "\n# edit\n"},
		{name: "no container", compose: services.ServiceMap["tei"], state: map[string]string{}, wantFile: services.ServiceMap["tei"]},
		{name: "node-dir override", compose: services.ServiceMap["tei"], state: map[string]string{"citadel-tei": "running"}, override: true, wantFile: services.ServiceMap["tei"]},
		{name: "recreate fails", compose: old, state: map[string]string{"citadel-tei": "running"}, runErr: errors.New("boom"), wantFile: old},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeManagedCompose{state: tc.state, runErr: tc.runErr}
			installFakeManagedCompose(t, f)
			dir, path := writeNodeFixture(t, "tei", tc.compose, "")
			if tc.override {
				t.Setenv("CITADEL_NODE_DIR", dir)
			}
			svc := Service{Name: "tei", ComposeFile: "services/tei.yml"}
			handled, err := restartByRecreatingManagedService(managedCompose.runtime(), svc, path)
			if (err != nil) != (tc.runErr != nil) || handled != tc.wantHandled {
				t.Fatalf("handled = %v, err = %v; want handled=%v", handled, err, tc.wantHandled)
			}
			wantRuns := 0
			if tc.wantHandled || tc.runErr != nil {
				wantRuns = 1
			}
			if len(f.runs) != wantRuns || (wantRuns == 1 && !hasArg(f.runs[0], "--force-recreate")) {
				t.Fatalf("runs = %v, want %d forced recreate(s)", f.runs, wantRuns)
			}
			if got, _ := os.ReadFile(path); string(got) != tc.wantFile {
				t.Fatalf("file after restart:\n%s", got)
			}
		})
	}
}

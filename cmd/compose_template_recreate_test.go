package cmd

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/compose"
	"github.com/aceteam-ai/citadel-cli/internal/compose/composefake"
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

// fakeManagedCompose installs a composefake engine behind managedCompose, so
// tests assert on container state as well as on the commands run.
type fakeManagedCompose struct {
	engineBin string
	ports     map[string]int
	stateHit  int
	*composefake.Engine
}

func installFakeManagedCompose(t *testing.T, f *fakeManagedCompose) {
	t.Helper()
	t.Setenv("CITADEL_NODE_DIR", "")
	t.Setenv("CITADEL_WORKSPACE", t.TempDir())
	t.Setenv("CITADEL_COMPOSE_NO_RECREATE_ON_UPGRADE", "")
	prev, prevDelay := managedCompose, compose.ReplaceSettleDelay
	compose.ReplaceSettleDelay = 0
	t.Cleanup(func() { managedCompose, compose.ReplaceSettleDelay = prev, prevDelay })
	if f.engineBin == "" {
		f.engineBin = "docker"
	}
	if f.Engine == nil {
		f.Engine = composefake.New("tei", "citadel-tei", "tei:pinned")
	}
	managedCompose = managedComposeDeps{
		runtime: func() catalog.ContainerRuntime {
			return catalog.ContainerRuntime{EngineBin: f.engineBin, Bin: f.engineBin, ComposePrefix: []string{"compose"}}
		},
		containerState: func(_, name string) (string, bool) {
			f.stateHit++
			c, ok := f.Named(name)
			return c.State, ok
		},
		publishedPort: func(_, name string) (int, bool) {
			p, ok := f.ports[name]
			return p, ok
		},
		serviceFor: manifestServiceAt,
		exec:       f.Exec,
	}
}

// ups counts compose up invocations.
func (f *fakeManagedCompose) ups() int { return f.Count("compose", "up", "-d") }

// addRunning places the service's current container, created from path.
func (f *fakeManagedCompose) addRunning(project, path string) composefake.Container {
	return *f.Add(composefake.Container{Name: f.ContainerName, State: "running", Project: project, Service: f.Service, Config: composefake.FileHash(path)})
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
		state     string // "" = no container
		wantUps   int
		wantForce bool
	}{
		{name: "running docker", state: "running", wantUps: 1},
		{name: "running podman forces", engine: "podman", state: "running", wantUps: 1, wantForce: true},
		{name: "exited", state: "exited"},
		{name: "absent"},
		{name: "operator stopped", desired: "stopped", state: "running"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeManagedCompose{engineBin: tc.engine}
			installFakeManagedCompose(t, f)
			_, path := writeNodeFixture(t, "tei", prePinTEICompose(t), tc.desired)
			var old composefake.Container
			if tc.state != "" {
				old = f.addRunning("services", path)
				if tc.state != "running" {
					_, _, _ = f.Exec(exec.Command("docker", "stop", old.ID))
				}
			}
			if err := os.WriteFile(path, []byte(services.ServiceMap["tei"]), 0600); err != nil {
				t.Fatal(err)
			}
			before := f.Containers()
			recreated, err := managedComposeRecreatorUnchecked("tei", path, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			if f.ups() != tc.wantUps || recreated != (tc.wantUps == 1) {
				t.Fatalf("ups = %d, recreated = %v; want %d; calls %v", f.ups(), recreated, tc.wantUps, f.Calls)
			}
			if tc.desired == "stopped" && f.stateHit != 0 {
				t.Fatal("stopped service was inspected")
			}
			if tc.wantUps == 0 {
				if after := f.Containers(); fmt.Sprint(after) != fmt.Sprint(before) {
					t.Fatalf("containers changed without a recreate: %+v -> %+v", before, after)
				}
				return
			}
			if (f.Count("up", "--force-recreate") == 1) != tc.wantForce {
				t.Fatalf("force = %v, want %v: %v", !tc.wantForce, tc.wantForce, f.Calls)
			}
			c, ok := f.Named("citadel-tei")
			if !ok || c.ID == old.ID || c.State != "running" || c.Config != composefake.FileHash(path) {
				t.Fatalf("container after recreate = %+v; want a new one running the refreshed file", c)
			}
			if f.Count("rename") != 0 || f.Count("rm") != 0 {
				t.Fatalf("same-project container renamed or removed by hand: %v", f.Calls)
			}
		})
	}
}

func TestManagedComposeRecreatorForcesWhenHostPortMoved(t *testing.T) {
	for _, tc := range []struct {
		port      int
		wantForce bool
	}{{port: 8080, wantForce: true}, {port: 8200}} {
		f := &fakeManagedCompose{Engine: composefake.New("llamacpp", "citadel-llamacpp", "llamacpp:x"), ports: map[string]int{"citadel-llamacpp": tc.port}}
		installFakeManagedCompose(t, f)
		_, path := writeNodeFixture(t, "llamacpp", services.ServiceMap["llamacpp"], "")
		f.addRunning("services", path)
		if _, err := managedComposeRecreatorUnchecked("llamacpp", path, 8200, nil); err != nil {
			t.Fatal(err)
		}
		if f.ups() != 1 || (f.Count("up", "--force-recreate") == 1) != tc.wantForce {
			t.Fatalf("port %d: calls = %v, want force=%v", tc.port, f.Calls, tc.wantForce)
		}
	}
}

// The boot sweep applies a refreshed TEI template to the running container
// exactly once: the same binary's next boot skips the sweep, an operator-edited
// file is preserved and not recreated, and the opt-out leaves it alone.
func TestBootRefreshRecreatesRunningTEIOnceAndNeverInALoop(t *testing.T) {
	old := prePinTEICompose(t)
	for _, tc := range []struct {
		name    string
		compose string
		optOut  bool
		wantUps int
	}{
		{name: "pre-pin template", compose: old, wantUps: 1},
		{name: "operator edited", compose: old + "\n# operator change\n"},
		{name: "opted out", compose: old, optOut: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeManagedCompose{}
			installFakeManagedCompose(t, f)
			if tc.optOut {
				t.Setenv("CITADEL_COMPOSE_NO_RECREATE_ON_UPGRADE", "1")
			}
			dir, path := writeNodeFixture(t, "tei", tc.compose, "")
			f.addRunning("services", path)
			for i := 0; i < 2; i++ {
				refreshManagedComposeFilesWithSweep(dir, composerefresh.Sweep, func() int { return 0 })
			}
			if f.ups() != tc.wantUps {
				t.Fatalf("compose ups = %d, want %d: %v", f.ups(), tc.wantUps, f.Calls)
			}
			got, _ := os.ReadFile(path)
			if edited := tc.compose != old; edited != (string(got) == tc.compose) {
				t.Fatalf("file after sweep (edited=%v):\n%s", edited, got)
			}
		})
	}
}

// A failed boot recreate restores the previous file and leaves the service
// running: a legacy-project container comes back under its own name and ID; a
// same-project container compose already removed is started again from the
// previous file.
func TestBootRefreshRecreateFailureKeepsServiceRunning(t *testing.T) {
	old := prePinTEICompose(t)
	for _, project := range []string{"citadel-tei", "services"} {
		t.Run(project, func(t *testing.T) {
			f := &fakeManagedCompose{}
			installFakeManagedCompose(t, f)
			dir, path := writeNodeFixture(t, "tei", old, "")
			prev := f.addRunning(project, path)
			f.Fail["up-start"] = 1
			refreshManagedComposeFilesWithSweep(dir, composerefresh.Sweep, func() int { return 0 })
			if got, _ := os.ReadFile(path); string(got) != old {
				t.Fatalf("file not rolled back:\n%s", got)
			}
			all := f.Containers()
			if len(all) != 1 || all[0].Name != "citadel-tei" || all[0].State != "running" || all[0].Config != composefake.FileHash(path) {
				t.Fatalf("containers after failed recreate = %+v; want the service running its previous file", all)
			}
			if project == "citadel-tei" && (all[0].ID != prev.ID || all[0].Project != "citadel-tei") {
				t.Fatalf("legacy container not restored: %+v (was %+v)", all[0], prev)
			}
		})
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
	current := services.ServiceMap["tei"]
	for _, tc := range []struct {
		name        string
		compose     string
		project     string // "" = no container
		override    bool
		fail        bool
		wantHandled bool
		wantFile    string
	}{
		{name: "pre-pin legacy running", compose: old, project: "citadel-tei", wantHandled: true, wantFile: current},
		{name: "current same project", compose: current, project: "services", wantHandled: true, wantFile: current},
		{name: "operator edited", compose: old + "\n# edit\n", project: "services", wantFile: old + "\n# edit\n"},
		{name: "no container", compose: current, wantFile: current},
		{name: "node-dir override", compose: current, project: "services", override: true, wantFile: current},
		{name: "legacy replacement fails", compose: old, project: "citadel-tei", fail: true, wantHandled: true, wantFile: old},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeManagedCompose{}
			installFakeManagedCompose(t, f)
			dir, path := writeNodeFixture(t, "tei", tc.compose, "")
			var prev composefake.Container
			if tc.project != "" {
				prev = f.addRunning(tc.project, path)
			}
			if tc.override {
				t.Setenv("CITADEL_NODE_DIR", dir)
			}
			if tc.fail {
				f.Fail["up-start"] = 1
			}
			svc := Service{Name: "tei", ComposeFile: "services/tei.yml"}
			handled, err := restartByRecreatingManagedService(managedCompose.runtime(), svc, path)
			if (err != nil) != tc.fail || handled != tc.wantHandled {
				t.Fatalf("handled = %v, err = %v; want handled=%v fail=%v", handled, err, tc.wantHandled, tc.fail)
			}
			if got, _ := os.ReadFile(path); string(got) != tc.wantFile {
				t.Fatalf("file after restart:\n%s", got)
			}
			c, _ := f.Named("citadel-tei")
			switch {
			case !tc.wantHandled:
				if len(f.Calls) > 0 && f.ups() > 0 {
					t.Fatalf("unhandled restart ran compose up: %v", f.Calls)
				}
				if tc.project != "" && c != prev {
					t.Fatalf("container changed: %+v", c)
				}
			case tc.fail:
				if c.ID != prev.ID || c.State != "running" || len(f.Containers()) != 1 {
					t.Fatalf("after a failed restart the old container is not back: %+v", f.Containers())
				}
			default:
				if c.ID == prev.ID || c.State != "running" || len(f.Containers()) != 1 || f.Count("up", "--force-recreate") != 1 {
					t.Fatalf("restart did not leave exactly one new running container: %+v %v", f.Containers(), f.Calls)
				}
				if tc.project == "citadel-tei" && f.Count("rm", "-f", prev.ID) != 1 {
					t.Fatalf("legacy backup not removed: %v", f.Calls)
				}
			}
		})
	}
}

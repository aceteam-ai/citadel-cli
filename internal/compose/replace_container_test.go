package compose

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/compose/composefake"
)

const (
	oldCompose = "services:\n  tei:\n    command: [old]\n"
	newCompose = "services:\n  tei:\n    command: [new]\n"
)

type replaceFixture struct {
	engine   *composefake.Engine
	path     string
	restored int
}

func newReplaceFixture(t *testing.T) *replaceFixture {
	t.Helper()
	prev := ReplaceSettleDelay
	ReplaceSettleDelay = 0
	t.Cleanup(func() { ReplaceSettleDelay = prev })
	path := filepath.Join(t.TempDir(), "tei.yml")
	if err := os.WriteFile(path, []byte(oldCompose), 0600); err != nil {
		t.Fatal(err)
	}
	return &replaceFixture{engine: composefake.New("tei", "citadel-tei", "tei:1"), path: path}
}

// refresh writes the new template, as the caller's refresh did before replacing.
func (f *replaceFixture) refresh(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(f.path, []byte(newCompose), 0600); err != nil {
		t.Fatal(err)
	}
}

func (f *replaceFixture) spec(preflight func() error) ReplaceSpec {
	return ReplaceSpec{
		Runtime:       catalog.ContainerRuntime{EngineBin: "docker", Bin: "docker", ComposePrefix: []string{"compose"}},
		Service:       "tei",
		ContainerName: "citadel-tei",
		Project:       "services",
		ComposeArgs: func() ([]string, func(), error) {
			return []string{"-f", f.path}, func() {}, nil
		},
		Preflight: preflight,
		RestoreFile: func() error {
			f.restored++
			return os.WriteFile(f.path, []byte(oldCompose), 0600)
		},
		Exec: f.engine.Exec,
	}
}

func (f *replaceFixture) file(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (f *replaceFixture) addOld(project, state string) composefake.Container {
	return *f.engine.Add(composefake.Container{Name: "citadel-tei", State: state, Project: project, Service: "tei", Config: composefake.FileHash(f.path)})
}

func TestReplaceLegacyContainerSucceedsAndRemovesBackup(t *testing.T) {
	f := newReplaceFixture(t)
	old := f.addOld("citadel-tei", "running")
	f.refresh(t)
	outcome, err := ReplaceRunningService(f.spec(nil))
	if err != nil || outcome != ReplaceReplaced {
		t.Fatalf("outcome = %v, %v; want replaced", outcome, err)
	}
	all := f.engine.Containers()
	if len(all) != 1 || all[0].Name != "citadel-tei" || all[0].ID == old.ID || all[0].State != "running" || all[0].Project != "services" {
		t.Fatalf("containers after success = %+v; want only the new running one", all)
	}
	if f.engine.Count("rename", old.ID, BackupContainerName("citadel-tei")) != 1 || f.engine.Count("rm", "-f", old.ID) != 1 {
		t.Fatalf("backup not renamed and removed: %v", f.engine.Calls)
	}
}

func TestReplaceLegacyFailureRestoresOldContainerRunning(t *testing.T) {
	for _, step := range []string{"up", "up-start", "exit-after-start"} {
		t.Run(step, func(t *testing.T) {
			f := newReplaceFixture(t)
			old := f.addOld("citadel-tei", "running")
			f.refresh(t)
			f.engine.Fail[step] = 1
			outcome, err := ReplaceRunningService(f.spec(nil))
			if err == nil || outcome != ReplaceRestored {
				t.Fatalf("outcome = %v, %v; want restored with an error", outcome, err)
			}
			all := f.engine.Containers()
			if len(all) != 1 || all[0].ID != old.ID || all[0].Name != "citadel-tei" || all[0].State != "running" || all[0].Project != "citadel-tei" {
				t.Fatalf("containers after failure = %+v; want the old one running under its name", all)
			}
			if f.file(t) != oldCompose || f.restored != 1 {
				t.Fatalf("file not restored (restores=%d)", f.restored)
			}
		})
	}
}

func TestReplaceRenameFailureReportsRestartFailure(t *testing.T) {
	for _, restartFails := range []bool{false, true} {
		f := newReplaceFixture(t)
		old := f.addOld("citadel-tei", "running")
		f.refresh(t)
		f.engine.Fail["rename"] = 1
		if restartFails {
			f.engine.Fail["start"] = 1
		}
		outcome, err := ReplaceRunningService(f.spec(nil))
		if err == nil || outcome != map[bool]ReplaceOutcome{false: ReplaceUnchanged, true: ReplaceFailed}[restartFails] {
			t.Fatalf("restartFails=%v: outcome=%v, err=%v", restartFails, outcome, err)
		}
		if got, ok := f.engine.Named("citadel-tei"); !ok || got.ID != old.ID || (got.State == "running") == restartFails || f.file(t) != oldCompose {
			t.Fatalf("restartFails=%v: prior state not reported correctly: %+v", restartFails, got)
		}
	}
}

func TestReplaceLegacyStoppedContainerIsNotStartedOnFailure(t *testing.T) {
	f := newReplaceFixture(t)
	old := f.addOld("citadel-tei", "exited")
	f.refresh(t)
	f.engine.Fail["up"] = 1
	if _, err := ReplaceRunningService(f.spec(nil)); err == nil {
		t.Fatal("want failure")
	}
	c, ok := f.engine.Named("citadel-tei")
	if !ok || c.ID != old.ID || c.State != "exited" {
		t.Fatalf("old stopped container = %+v; want it back, still stopped", c)
	}
	if f.engine.Count("stop") != 0 || f.engine.Count("start") != 0 {
		t.Fatalf("stopped container was stopped or started: %v", f.engine.Calls)
	}
}

func TestReplacePreflightFailureTouchesNothing(t *testing.T) {
	for name, setup := range map[string]func(f *replaceFixture) func() error{
		"resource preflight": func(*replaceFixture) func() error {
			return func() error { return errors.New("memory controller missing") }
		},
		"compose config": func(f *replaceFixture) func() error { f.engine.Fail["config"] = 1; return nil },
		"image pull": func(f *replaceFixture) func() error {
			f.engine.Images = map[string]bool{}
			f.engine.Fail["pull"] = 1
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newReplaceFixture(t)
			old := f.addOld("citadel-tei", "running")
			f.refresh(t)
			preflight := setup(f)
			outcome, err := ReplaceRunningService(f.spec(preflight))
			if err == nil || outcome != ReplaceUnchanged {
				t.Fatalf("outcome = %v, %v; want unchanged with an error", outcome, err)
			}
			all := f.engine.Containers()
			if len(all) != 1 || all[0] != old {
				t.Fatalf("containers after preflight failure = %+v; want untouched %+v", all, old)
			}
			for _, verb := range []string{"stop", "rename", "rm", "up"} {
				if f.engine.Count(verb) != 0 {
					t.Fatalf("%s ran before preflight passed: %v", verb, f.engine.Calls)
				}
			}
			if f.restored != 1 || f.file(t) != oldCompose {
				t.Fatal("previous compose file was not restored after preflight failure")
			}
		})
	}
}

func TestReplaceInspectUncertainFailsClosed(t *testing.T) {
	for _, failure := range []string{"inspect", "inspect-malformed"} {
		t.Run(failure, func(t *testing.T) {
			f := newReplaceFixture(t)
			old := f.addOld("citadel-tei", "running")
			f.refresh(t)
			f.engine.Fail[failure] = 1
			outcome, err := ReplaceRunningService(f.spec(nil))
			if err == nil || outcome != ReplaceUnchanged {
				t.Fatalf("outcome = %v, %v; want refusal", outcome, err)
			}
			if all := f.engine.Containers(); len(all) != 1 || all[0] != old || f.engine.Count("up") != 0 {
				t.Fatalf("uncertain inspect changed service: %+v %v", all, f.engine.Calls)
			}
		})
	}
}

func TestReplaceBackupPreflightFailureDoesNotMutate(t *testing.T) {
	f := newReplaceFixture(t)
	backup := *f.engine.Add(stoppedContainer(BackupContainerName("citadel-tei"), "citadel-tei"))
	f.refresh(t)
	f.engine.Fail["config"] = 1
	if _, err := ReplaceRunningService(f.spec(nil)); err == nil {
		t.Fatal("preflight should fail")
	}
	if got, ok := f.engine.Named(backup.Name); !ok || got != backup || f.engine.Count("rename") != 0 || f.file(t) != oldCompose {
		t.Fatalf("backup or file changed before preflight: %+v %v", got, f.engine.Calls)
	}
}

func TestReplaceForeignSameServiceNeverRemovesBackup(t *testing.T) {
	f := newReplaceFixture(t)
	foreign := *f.engine.Add(composefake.Container{Name: "citadel-tei", State: "running", Project: "foreign", Service: "tei"})
	backup := *f.engine.Add(stoppedContainer(BackupContainerName("citadel-tei"), "citadel-tei"))
	if _, err := ReplaceRunningService(f.spec(nil)); err == nil {
		t.Fatal("foreign project accepted")
	}
	if all := f.engine.Containers(); len(all) != 2 || all[0] != foreign || all[1] != backup || f.engine.Count("rm") != 0 {
		t.Fatalf("foreign project caused backup deletion: %+v", all)
	}
}

func TestReplaceMissingImageIsPulledOnlyWhenAbsent(t *testing.T) {
	f := newReplaceFixture(t)
	f.addOld("citadel-tei", "running")
	f.refresh(t)
	if _, err := ReplaceRunningService(f.spec(nil)); err != nil {
		t.Fatal(err)
	}
	if f.engine.Count("pull") != 0 {
		t.Fatalf("present image was pulled: %v", f.engine.Calls)
	}
	g := newReplaceFixture(t)
	g.addOld("citadel-tei", "running")
	g.engine.Images = map[string]bool{}
	g.refresh(t)
	if _, err := ReplaceRunningService(g.spec(nil)); err != nil {
		t.Fatal(err)
	}
	if g.engine.Count("pull", "tei") != 1 {
		t.Fatalf("missing image was not pulled once: %v", g.engine.Calls)
	}
}

func TestReplaceSameProjectFailsClosed(t *testing.T) {
	f := newReplaceFixture(t)
	old := f.addOld("services", "running")
	f.refresh(t)
	outcome, err := ReplaceRunningService(f.spec(nil))
	if err == nil || outcome != ReplaceUnchanged || !strings.Contains(err.Error(), "same-project") {
		t.Fatalf("outcome = %v, %v; want safe refusal", outcome, err)
	}
	if all := f.engine.Containers(); len(all) != 1 || all[0] != old || f.file(t) != oldCompose {
		t.Fatalf("same-project container or file changed: %+v", all)
	}
	if f.engine.Count("up") != 0 || f.engine.Count("stop") != 0 || f.engine.Count("rm") != 0 {
		t.Fatalf("same-project container touched: %v", f.engine.Calls)
	}
}

func TestReplaceNoContainerAndForeignContainer(t *testing.T) {
	f := newReplaceFixture(t)
	f.refresh(t)
	outcome, err := ReplaceRunningService(f.spec(nil))
	if err != nil || outcome != ReplaceReplaced {
		t.Fatalf("no container = %v, %v; want a fresh container", outcome, err)
	}
	if f.engine.Count("rename") != 0 || f.engine.Count("rm") != 0 || f.engine.Count("stop") != 0 {
		t.Fatalf("no legacy container, yet rename/rm/stop ran: %v", f.engine.Calls)
	}

	g := newReplaceFixture(t)
	foreign := *g.engine.Add(composefake.Container{Name: "citadel-tei", State: "running", Project: "other", Service: "web"})
	if _, err := ReplaceRunningService(g.spec(nil)); err == nil {
		t.Fatal("foreign container accepted")
	}
	if all := g.engine.Containers(); len(all) != 1 || all[0] != foreign || len(g.engine.Calls) > 2 {
		t.Fatalf("foreign container touched: %+v %v", all, g.engine.Calls)
	}
}

func TestReplaceRestoresBackupLeftByInterruptedRun(t *testing.T) {
	f := newReplaceFixture(t)
	old := *f.engine.Add(composefake.Container{Name: BackupContainerName("citadel-tei"), State: "exited", Project: "citadel-tei", Service: "tei"})
	f.engine.Fail["up"] = 1
	if _, err := ReplaceRunningService(f.spec(nil)); err == nil {
		t.Fatal("want failure")
	}
	c, ok := f.engine.Named("citadel-tei")
	if !ok || c.ID != old.ID {
		t.Fatalf("leftover backup not renamed back: %+v", f.engine.Containers())
	}
}

// A backup an earlier successful replacement could not remove does not make
// every later replacement refuse: it is removed by its labels while our
// replacement runs under the pinned name.
func TestReplaceRemovesBackupLeftByFailedCleanup(t *testing.T) {
	f := newReplaceFixture(t)
	old := f.addOld("citadel-tei", "running")
	f.refresh(t)
	f.engine.Fail["rm"] = 1
	if outcome, err := ReplaceRunningService(f.spec(nil)); err != nil || outcome != ReplaceReplaced {
		t.Fatalf("first replacement = %v, %v", outcome, err)
	}
	if _, ok := f.engine.Named(BackupContainerName("citadel-tei")); !ok {
		t.Fatal("fixture: backup removal did not fail")
	}
	if _, err := ReplaceRunningService(f.spec(nil)); err == nil || !strings.Contains(err.Error(), "same-project") {
		t.Fatalf("second replacement should preserve the running service: %v", err)
	}
	all := f.engine.Containers()
	if len(all) != 1 || all[0].Name != "citadel-tei" || all[0].ID == old.ID || all[0].State != "running" {
		t.Fatalf("containers = %+v; want only the running replacement", all)
	}

	// A backup next to a container that is not our running replacement stays.
	g := newReplaceFixture(t)
	g.addOld("services", "exited")
	backup := *g.engine.Add(stoppedContainer(BackupContainerName("citadel-tei"), "citadel-tei"))
	if _, err := ReplaceRunningService(g.spec(nil)); err == nil {
		t.Fatal("ambiguous backup accepted")
	}
	if b, ok := g.engine.Named(BackupContainerName("citadel-tei")); !ok || b != backup || g.engine.Count("rm") != 0 {
		t.Fatalf("ambiguous backup touched: %+v %v", b, g.engine.Calls)
	}
}

// stoppedContainer builds a stopped fake container for the named project.
func stoppedContainer(name, project string) composefake.Container {
	return composefake.Container{Name: name, State: "exited", Project: project, Service: "tei"}
}

// A failed recreate of a STOPPED same-project container that compose already
// removed is reported as a failure that removed it, not as restored.
func TestReplaceStoppedSameProjectRemovedIsNotRestored(t *testing.T) {
	f := newReplaceFixture(t)
	f.addOld("services", "exited")
	f.refresh(t)
	f.engine.Fail["up-start"] = 1
	outcome, err := ReplaceRunningService(f.spec(nil))
	if outcome != ReplaceFailed || err == nil || !strings.Contains(err.Error(), "removed by compose") {
		t.Fatalf("outcome = %v, %v; want failed, reporting the removal", outcome, err)
	}
	if len(f.engine.Containers()) != 0 || f.engine.Count("--force-recreate") != 0 {
		t.Fatalf("a stopped service was recreated or left partial: %+v %v", f.engine.Containers(), f.engine.Calls)
	}
	if f.file(t) != oldCompose {
		t.Fatal("file not restored")
	}
}

func TestReplaceInspectsContainersOnly(t *testing.T) {
	f := newReplaceFixture(t)
	f.addOld("citadel-tei", "running")
	f.refresh(t)
	if _, err := ReplaceRunningService(f.spec(nil)); err != nil {
		t.Fatal(err)
	}
	for _, call := range f.engine.Calls {
		if strings.HasPrefix(call, "docker inspect") {
			t.Fatalf("bare inspect used: %s", call)
		}
	}
}

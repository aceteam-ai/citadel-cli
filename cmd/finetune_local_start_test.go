package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/finetunesafety"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/reconcile"
)

func heldFineTuneModuleFixture(t *testing.T) string {
	t.Helper()
	configDir := writeManifestWithServices(t, []Service{
		{Name: "unlimited-ocr", Type: "docker", ComposeFile: filepath.Join("services", "unlimited-ocr.yml"), DesiredStatus: "stopped", EvictedByJob: "train-job"},
		{Name: "paw-compile", Type: "docker", ComposeFile: filepath.Join("services", "paw-compile.yml"), DesiredStatus: "stopped"},
	})
	if err := os.MkdirAll(filepath.Join(configDir, "services"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "services", "paw-compile.yml"), []byte("services:\n  paw-compile:\n    image: example/paw-compile\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(finetunesafety.Dir(configDir), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finetunesafety.Path(configDir), []byte("train-job\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return configDir
}

func assertHeldTagUnchanged(t *testing.T, configDir string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(configDir, "citadel.yaml"))
	if err != nil || !strings.Contains(string(data), "evicted_by_job: train-job") || !strings.Contains(string(data), "desired_status: stopped") {
		t.Fatalf("held reservation changed: %s, %v", data, err)
	}
}

func TestFineTuneHeldTagBlocksModuleStartRestartAndUninstall(t *testing.T) {
	configDir := heldFineTuneModuleFixture(t)
	ops, calls := newControlTestOps(map[string]bool{"unlimited-ocr": false})
	for _, action := range []struct {
		name string
		run  func() error
	}{
		{"start", func() error { return ops.Start(context.Background(), "unlimited-ocr") }},
		{"restart", func() error { return dispatchModuleAction(ops, "unlimited-ocr", moduleActionRestart) }},
		{"uninstall", func() error { return ops.Uninstall(context.Background(), "unlimited-ocr") }},
	} {
		if err := action.run(); err == nil || !strings.Contains(err.Error(), "reserved by active fine-tune") {
			t.Fatalf("%s under held tag = %v, want refusal", action.name, err)
		}
		assertHeldTagUnchanged(t, configDir)
	}
	for _, call := range *calls {
		if strings.HasPrefix(call, "start:") {
			t.Fatalf("held module started: %v", *calls)
		}
	}
}

func TestFineTuneHeldTagBlocksInstallUpdateBeforeTagErasure(t *testing.T) {
	configDir := heldFineTuneModuleFixture(t)
	ops, calls := newControlTestOps(map[string]bool{})
	ops.resolveSource = func(catalog.Source) (*catalog.ServiceManifest, string, *catalog.ResolvedModule, error) {
		return &catalog.ServiceManifest{Name: "unlimited-ocr"}, "", nil, nil
	}
	if err := ops.Install(context.Background(), reconcile.ModuleAssignment{Source: "unlimited-ocr"}); err == nil || !strings.Contains(err.Error(), "reserved by active fine-tune") {
		t.Fatalf("install update under held tag = %v, want refusal before uninstall", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("held install update touched service: %v", *calls)
	}
	assertHeldTagUnchanged(t, configDir)
}

func TestFineTuneHeldTagAllowsUnrelatedModuleStart(t *testing.T) {
	heldFineTuneModuleFixture(t)
	ops, calls := newControlTestOps(map[string]bool{})
	if err := ops.Start(context.Background(), "paw-compile"); err != nil {
		t.Fatalf("unrelated module start: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0] != "start:paw-compile" {
		t.Fatalf("unrelated start calls = %v", *calls)
	}
}

func TestFineTuneNoHoldAllowsNormalModuleStart(t *testing.T) {
	writeManifestWithServices(t, []Service{{Name: "ollama", Type: "docker", ComposeFile: filepath.Join("services", "ollama.yml"), DesiredStatus: "stopped"}})
	ops, calls := newControlTestOps(map[string]bool{})
	if err := ops.Start(context.Background(), "ollama"); err != nil {
		t.Fatalf("normal module start without hold: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0] != "start:ollama" {
		t.Fatalf("normal no-hold calls = %v", *calls)
	}
}

func TestFineTuneHoldBlocksCandidateThatWasAlreadyStopped(t *testing.T) {
	configDir := writeManifestWithServices(t, []Service{{Name: "ollama", Type: "docker", ComposeFile: filepath.Join("services", "ollama.yml"), DesiredStatus: "stopped"}})
	if err := os.MkdirAll(finetunesafety.Dir(configDir), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finetunesafety.Path(configDir), []byte("train-job\n"), 0600); err != nil {
		t.Fatal(err)
	}
	started := false
	if err := withLocalServiceStartGuard(configDir, "ollama", func() error {
		started = true
		return nil
	}); err == nil || started {
		t.Fatalf("already-stopped fine-tune candidate start = (started %v, err %v)", started, err)
	}
}

func TestFineTuneHoldBlocksAllUntaggedGPUStartsButAllowsCPU(t *testing.T) {
	configDir := heldFineTuneModuleFixture(t)
	for _, name := range []string{"vllm", "sglang", "llamacpp", "bonsai", "diffusers", "omnivoice"} {
		t.Run(name, func(t *testing.T) {
			called := false
			err := withLocalServiceStartGuard(configDir, name, func() error { called = true; return nil })
			if err == nil || called {
				t.Fatalf("untagged GPU start: called=%t err=%v", called, err)
			}
		})
	}
	called := false
	if err := withLocalServiceStartGuard(configDir, "paw-compile", func() error { called = true; return nil }); err != nil || !called {
		t.Fatalf("CPU service start: called=%t err=%v", called, err)
	}
}

func TestFineTuneHoldArmCannotInterleaveWithUntaggedGPUStart(t *testing.T) {
	configDir := writeManifestWithServices(t, []Service{{Name: "vllm", Type: "docker", ComposeFile: "services/vllm.yml"}})
	entered := make(chan struct{})
	finish := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withLocalServiceStartGuard(configDir, "vllm", func() error {
			close(entered)
			<-finish
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("start callback not reached")
	}
	if err := finetunesafety.WithExclusive(finetunesafety.Dir(configDir), func() error {
		return os.WriteFile(finetunesafety.Path(configDir), []byte("train-job"), 0600)
	}); err == nil {
		t.Fatal("fine-tune hold armed while GPU start in progress")
	}
	close(finish)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("start did not complete")
	}
	if err := finetunesafety.WithExclusive(finetunesafety.Dir(configDir), func() error {
		return os.WriteFile(finetunesafety.Path(configDir), []byte("train-job"), 0600)
	}); err != nil {
		t.Fatal(err)
	}
	if err := withLocalServiceStartGuard(configDir, "vllm", func() error { t.Fatal("GPU callback reached under hold"); return nil }); err == nil {
		t.Fatal("GPU start admitted after hold")
	}
}

func TestFineTuneHeldTagBlocksRunAndLocalMCPDeploy(t *testing.T) {
	configDir := heldFineTuneModuleFixture(t)
	started := false
	if err := startRunService(configDir, "unlimited-ocr", filepath.Join(configDir, "services", "unlimited-ocr.yml"), func(string, string) error {
		started = true
		return nil
	}); err == nil || started {
		t.Fatalf("citadel run held service = (started %v, err %v)", started, err)
	}
	if _, err := realLocalReservationOps().deploy("local-job", "unlimited-ocr", "", 0); err == nil || !strings.Contains(err.Error(), "reserved by active fine-tune") {
		t.Fatalf("local_model_deploy held service = %v, want refusal", err)
	}
	assertHeldTagUnchanged(t, configDir)
	if err := startRunService(configDir, "paw-compile", filepath.Join(configDir, "services", "paw-compile.yml"), func(string, string) error {
		started = true
		return nil
	}); err != nil || !started {
		t.Fatalf("citadel run unrelated service = (started %v, err %v)", started, err)
	}
}

func TestLocalStartAndFineTuneReservationCannotInterleave(t *testing.T) {
	configDir := heldFineTuneModuleFixture(t)
	starting := make(chan struct{})
	finishStart := make(chan struct{})
	startDone := make(chan error, 1)
	go func() {
		startDone <- withLocalServiceStartGuard(configDir, "paw-compile", func() error {
			close(starting)
			<-finishStart
			return nil
		})
	}()
	<-starting
	reservation := &fineTuneServiceReservation{service: jobs.NewServiceHandler(configDir)}
	if evicted, err := reservation.Reserve(context.Background(), "train-job"); err == nil || len(evicted) != 0 {
		t.Fatalf("fine-tune reservation during local start = (%v, %v), want lock refusal", evicted, err)
	}
	close(finishStart)
	if err := <-startDone; err != nil {
		t.Fatal(err)
	}
	assertHeldTagUnchanged(t, configDir)
}

func TestFineTuneHeldTagBlocksUpdateRefreshAndGatewaySwapStarts(t *testing.T) {
	configDir := heldFineTuneModuleFixture(t)
	composePath := filepath.Join(configDir, "services", "unlimited-ocr.yml")
	if err := composeUpDetached("unlimited-ocr", composePath); err == nil || !strings.Contains(err.Error(), "reserved by active fine-tune") {
		t.Fatalf("module update restart under hold = %v", err)
	}
	if recreated, err := enginePortRecreator("unlimited-ocr", composePath, 18080); err == nil || recreated {
		t.Fatalf("compose refresh recreate under hold = (%v, %v)", recreated, err)
	}
	swap := newSwapController(configDir, t.TempDir(), nil, nil)
	if err := swap.Start(context.Background(), "unlimited-ocr", ""); err == nil || !strings.Contains(err.Error(), "reserved by active fine-tune") {
		t.Fatalf("gateway swap start under hold = %v", err)
	}
	assertHeldTagUnchanged(t, configDir)
}

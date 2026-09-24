package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/composerefresh"
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

func TestFineTuneIncomingGPUInstallRefusesBeforeMutation(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "cpu-to-gpu"}[existing], func(t *testing.T) {
			services := []Service{{Name: "paw-compile", Type: "docker", ComposeFile: "services/paw-compile.yml"}}
			if existing {
				services = append(services, Service{Name: "custom-model", Type: "docker", ComposeFile: "services/custom-model.yml"})
			}
			configDir := writeManifestWithServices(t, services)
			if err := os.WriteFile(filepath.Join(configDir, "services", "paw-compile.yml"), []byte("services:\n  paw-compile:\n    image: example/cpu\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if existing {
				if err := os.WriteFile(filepath.Join(configDir, "services", "custom-model.yml"), []byte("services:\n  custom-model:\n    image: example/cpu\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			incoming := filepath.Join(t.TempDir(), "incoming.yml")
			if err := os.WriteFile(incoming, []byte("services:\n  custom-model:\n    image: example/gpu\n    gpus: all\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(finetunesafety.Dir(configDir), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(finetunesafety.Path(configDir), []byte("train-job"), 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(configDir, "citadel.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			ops, calls := newControlTestOps(map[string]bool{})
			ops.resolveSource = func(catalog.Source) (*catalog.ServiceManifest, string, *catalog.ResolvedModule, error) {
				return &catalog.ServiceManifest{Name: "custom-model"}, incoming, nil, nil
			}
			if err := ops.Install(context.Background(), reconcile.ModuleAssignment{Source: "custom-model"}); err == nil {
				t.Fatal("incoming GPU install admitted under hold")
			}
			if len(*calls) != 0 {
				t.Fatalf("incoming GPU install touched runtime: %v", *calls)
			}
			after, err := os.ReadFile(filepath.Join(configDir, "citadel.yaml"))
			if err != nil || string(before) != string(after) {
				t.Fatalf("manifest changed: %v", err)
			}
			if existing {
				got, err := os.ReadFile(filepath.Join(configDir, "services", "custom-model.yml"))
				if err != nil || strings.Contains(string(got), "gpus:") {
					t.Fatalf("old CPU compose changed: %q %v", got, err)
				}
			} else if _, err := os.Stat(filepath.Join(configDir, "services", "custom-model.yml")); !os.IsNotExist(err) {
				t.Fatalf("fresh GPU compose materialized: %v", err)
			}
		})
	}
}

func TestFineTuneExplicitRunAndTUIAddRefuseBeforeMaterialization(t *testing.T) {
	configDir := writeManifestWithServices(t, nil)
	if err := os.MkdirAll(finetunesafety.Dir(configDir), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finetunesafety.Path(configDir), []byte("train-job"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(configDir, "citadel.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := runSingleServiceApply(configDir, "vllm", func(string, string) error { t.Fatal("run reached start"); return nil }); err == nil {
		t.Fatal("held run admitted")
	}
	if err := ccAddService("vllm"); err == nil {
		t.Fatal("held TUI add admitted")
	}
	if _, err := os.Stat(filepath.Join(configDir, "services", "vllm.yml")); !os.IsNotExist(err) {
		t.Fatalf("GPU compose materialized: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(configDir, "citadel.yaml"))
	if err != nil || string(before) != string(after) {
		t.Fatalf("manifest changed: %v", err)
	}
}

// The global node pointer, not a successfully parsed manifest, determines
// which reservation.lock and active.hold protect local entrypoints. A damaged
// manifest must not make them bootstrap ~/citadel-node instead.
func TestFineTuneConfiguredNodeManifestFailureNeverFallsBack(t *testing.T) {
	for _, manifestState := range []string{"missing", "malformed"} {
		t.Run(manifestState, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CITADEL_NODE_DIR", "")
			configDir := filepath.Join(home, "configured-node")
			defaultDir := filepath.Join(home, "citadel-node")
			pointerPath := filepath.Join(home, ".citadel-cli", "config.yaml")
			if err := os.MkdirAll(filepath.Dir(pointerPath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(finetunesafety.Dir(configDir), 0700); err != nil {
				t.Fatal(err)
			}
			pointer := []byte("node_config_dir: " + configDir + "\n")
			if err := os.WriteFile(pointerPath, pointer, 0600); err != nil {
				t.Fatal(err)
			}
			hold := []byte("train-job\n")
			if err := os.WriteFile(finetunesafety.Path(configDir), hold, 0600); err != nil {
				t.Fatal(err)
			}
			// Warm the local catalog so each CLI/TUI installer reaches node-dir
			// admission without a network refresh or an unrelated source error.
			catalogServiceDir := filepath.Join(catalog.GetCatalogPath(), "services", "vllm")
			if err := os.MkdirAll(catalogServiceDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(catalogServiceDir, "service.yaml"), []byte("name: vllm\nversion: 1.0.0\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(catalogServiceDir, "compose.yml"), []byte("services:\n  vllm:\n    image: example/vllm\n    gpus: all\n"), 0600); err != nil {
				t.Fatal(err)
			}
			manifestPath := filepath.Join(configDir, "citadel.yaml")
			if manifestState == "malformed" {
				if err := os.WriteFile(manifestPath, []byte("node: [\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}

			resolved, err := localServiceConfigDir()
			if err != nil || resolved != configDir {
				t.Fatalf("local node dir = %q, %v; want %q", resolved, err, configDir)
			}
			started := false
			if err := runSingleServiceApply(resolved, "vllm", func(string, string) error { started = true; return nil }); err == nil {
				t.Fatal("run admitted without a usable configured manifest")
			}
			if err := ccAddService("vllm"); err == nil {
				t.Fatal("TUI add admitted without a usable configured manifest")
			}
			if err := runCatalogInstall(nil, []string{"vllm"}); err == nil {
				t.Fatal("catalog install admitted without a usable configured manifest")
			}
			if err := runModuleInstall(nil, []string{"vllm"}); err == nil {
				t.Fatal("module install admitted without a usable configured manifest")
			}
			if _, err := buildModuleInstallCallbacks().Install("vllm", nil, false); err == nil {
				t.Fatal("TUI module install admitted without a usable configured manifest")
			}
			ops, calls := newControlTestOps(map[string]bool{})
			ops.resolveSource = func(catalog.Source) (*catalog.ServiceManifest, string, *catalog.ResolvedModule, error) {
				return &catalog.ServiceManifest{Name: "vllm"}, "", nil, nil
			}
			if err := ops.Install(context.Background(), reconcile.ModuleAssignment{Source: "vllm"}); err == nil {
				t.Fatal("MODULE_SET install admitted without a usable configured manifest")
			}
			if err := withLocalServiceMutationLock(resolved, func() error {
				_, _, err := findOrCreateManifest()
				return err
			}); err == nil {
				t.Fatal("CLI/TUI install bootstrap admitted without a usable configured manifest")
			}
			if started || len(*calls) != 0 {
				t.Fatalf("started=%t module calls=%v", started, *calls)
			}
			if _, err := os.Stat(filepath.Join(defaultDir, "citadel.yaml")); !os.IsNotExist(err) {
				t.Fatalf("default manifest created: %v", err)
			}
			if _, err := os.Stat(filepath.Join(configDir, "services", "vllm.yml")); !os.IsNotExist(err) {
				t.Fatalf("configured service materialized: %v", err)
			}
			if got, err := os.ReadFile(pointerPath); err != nil || string(got) != string(pointer) {
				t.Fatalf("global pointer changed: %q, %v", got, err)
			}
			if got, err := os.ReadFile(finetunesafety.Path(configDir)); err != nil || string(got) != string(hold) {
				t.Fatalf("active hold changed: %q, %v", got, err)
			}
			if manifestState == "missing" {
				if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
					t.Fatalf("configured manifest created: %v", err)
				}
			} else if got, err := os.ReadFile(manifestPath); err != nil || string(got) != "node: [\n" {
				t.Fatalf("configured manifest changed: %q, %v", got, err)
			}
		})
	}
}

func TestFineTuneCPUStartSerializesWithIncomingUpdateAndInstallWriter(t *testing.T) {
	configDir := heldFineTuneModuleFixture(t)
	path := filepath.Join(configDir, "services", "paw-compile.yml")
	incoming := filepath.Join(t.TempDir(), "gpu.yml")
	gpuCompose := []byte("services:\n  paw-compile:\n    image: example/gpu\n    gpus: all\n")
	if err := os.WriteFile(incoming, gpuCompose, 0600); err != nil {
		t.Fatal(err)
	}
	entered, finish := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withLocalServiceStartGuard(configDir, "paw-compile", func() error { close(entered); <-finish; return nil })
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("CPU start did not enter")
	}
	updateCalled := false
	if err := withModuleUpdateTransaction(configDir, "paw-compile", &catalog.ServiceManifest{Name: "paw-compile"}, incoming, func() error { updateCalled = true; return os.WriteFile(path, gpuCompose, 0600) }); err == nil || updateCalled {
		t.Fatalf("GPU update interleaved with CPU start: called=%t err=%v", updateCalled, err)
	}
	writerCalled := false
	if err := withLocalServiceMutationLock(configDir, func() error { writerCalled = true; return os.WriteFile(path, gpuCompose, 0600) }); err == nil || writerCalled {
		t.Fatalf("no-start installer overwrote CPU compose mid-start: called=%t err=%v", writerCalled, err)
	}
	close(finish)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CPU start hung")
	}
	if err := withModuleUpdateTransaction(configDir, "paw-compile", &catalog.ServiceManifest{Name: "paw-compile"}, incoming, func() error { t.Fatal("held GPU update mutation reached"); return nil }); err == nil {
		t.Fatal("GPU update admitted after CPU start")
	}
	if err := withLocalServiceMutationLock(configDir, func() error { return os.WriteFile(path, gpuCompose, 0600) }); err != nil {
		t.Fatal(err)
	}
	if err := withLocalServiceStartGuard(configDir, "paw-compile", func() error { t.Fatal("GPU file started after installer overwrite"); return nil }); err == nil {
		t.Fatal("GPU file did not invalidate subsequent start")
	}
}

func TestFineTuneHeldComposeRefreshDefersBeforeRewrite(t *testing.T) {
	configDir := heldFineTuneModuleFixture(t)
	called := 0
	sweep := func(composerefresh.Options) (composerefresh.Result, error) {
		called++
		return composerefresh.Result{}, nil
	}
	refreshManagedComposeFilesWithSweep(configDir, sweep)
	if called != 0 {
		t.Fatal("compose refresh rewrote under fine-tune hold")
	}
	if err := os.Remove(finetunesafety.Path(configDir)); err != nil {
		t.Fatal(err)
	}
	refreshManagedComposeFilesWithSweep(configDir, sweep)
	if called != 1 {
		t.Fatalf("compose refresh did not resume after hold: %d", called)
	}
}

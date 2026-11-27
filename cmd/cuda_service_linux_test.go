//go:build linux

package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/externalengine"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/services"
)

// These wiring regressions use isolated fixture state and injected GPU/runtime observations.
func TestBootNativeCUDAAdmission(t *testing.T) {
	for _, kind := range []string{"native", ""} {
		for _, tc := range []struct {
			name        string
			gpu         int
			serving     bool
			wantStarts  int
			wantRunning int
		}{
			{name: "absent", gpu: 0},
			{name: "unknown", gpu: -1, wantStarts: 1},
			{name: "present", gpu: 1, wantStarts: 1},
			{name: "already serving", gpu: 0, serving: true, wantRunning: 1},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				dir := writeManifestWithServices(t, []Service{{Name: "vllm", Type: kind}})
				bin := t.TempDir()
				// Auto-detection may inspect the synthetic executable's presence,
				// but the injected launcher must never execute it.
				if err := os.WriteFile(filepath.Join(bin, "vllm"), []byte("#!/bin/sh\nexit 97\n"), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin)
				before, err := os.ReadFile(filepath.Join(dir, "citadel.yaml"))
				if err != nil {
					t.Fatal(err)
				}
				starts := 0
				running := startManagedServicesForHost(context.Background(), func() int { return tc.gpu }, func(name string) bool { return tc.serving }, func(name, configDir string) error {
					starts++
					return fmt.Errorf("synthetic native launch refused")
				})
				if starts != tc.wantStarts || len(running) != tc.wantRunning {
					t.Fatalf("starts=%d running=%d; want %d/%d", starts, len(running), tc.wantStarts, tc.wantRunning)
				}
				after, err := os.ReadFile(filepath.Join(dir, "citadel.yaml"))
				if err != nil || string(after) != string(before) {
					t.Fatal("native admission changed manifest", err)
				}
				if _, err := os.Stat(filepath.Join(dir, "logs")); !os.IsNotExist(err) {
					t.Fatal("native admission created logs", err)
				}
			})
		}
	}
}

func TestBootNativeCPUDoesNotProbeGPU(t *testing.T) {
	for _, name := range []string{"ollama", "llamacpp"} {
		t.Run(name, func(t *testing.T) {
			writeManifestWithServices(t, []Service{{Name: name, Type: "native"}})
			starts := 0
			startManagedServicesForHost(context.Background(), func() int { t.Fatal("CPU engine probed for CUDA admission"); return 0 }, func(string) bool { return false }, func(string, string) error {
				starts++
				return fmt.Errorf("synthetic native launch refused")
			})
			if starts != 1 {
				t.Fatalf("CPU launch attempts=%d", starts)
			}
		})
	}
}

func TestBootSkipsCUDAEnginesWithOneClearLineEach(t *testing.T) {
	names := []string{"bonsai", "unlimited-ocr", "vllm", "sglang", "diffusers", "omnivoice"}
	var manifest []Service
	for _, name := range names {
		manifest = append(manifest, Service{Name: name, Type: "docker", ComposeFile: "services/" + name + ".yml"})
	}
	writeManifestWithServices(t, manifest)
	t.Setenv("PATH", t.TempDir())
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = previous; reader.Close(); writer.Close() })
	probes := 0
	started := startManagedServices(context.Background(), func() int { probes++; return 0 })
	writer.Close()
	os.Stdout = previous
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(started) != 0 || probes != 1 {
		t.Fatal("GPU-less boot attempted services or repeated probe", len(started), probes)
	}
	for _, name := range names {
		if strings.Count(string(output), name+" requires an NVIDIA GPU, not starting") != 1 {
			t.Fatal("missing/duplicate clear skip", name, string(output))
		}
		if strings.Contains(string(output), "Starting "+name+"...") {
			t.Fatal("unsupported engine attempted", name)
		}
	}
	if !strings.Contains(string(output), "0 started, 0 failed, 6 skipped") {
		t.Fatal("CUDA skips missing from startup summary", string(output))
	}
}

func TestCLIStartCUDAAbsenceAdoptsServingExternalEngine(t *testing.T) {
	t.Setenv("CITADEL_NODE_DIR", "")
	dir := writeManifestWithServices(t, []Service{{Name: "vllm", Type: "docker", ComposeFile: "services/vllm.yml"}})
	t.Setenv("PATH", t.TempDir())
	probed := false
	err := startServiceForHost("vllm", filepath.Join(dir, "services/vllm.yml"), func() int { return 0 }, func(name string) (string, bool) {
		probed = true
		if name != "vllm" {
			t.Fatal(name)
		}
		return "Adopted external vllm", true
	})
	if err != nil || !probed {
		t.Fatalf("adoption=%v err=%v", probed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "services/vllm.yml")); !os.IsNotExist(err) {
		t.Fatal("adoption created compose", err)
	}
}

func TestBootCUDAAbsenceAdoptsServingExternalEngine(t *testing.T) {
	t.Setenv("CITADEL_NODE_DIR", "")
	dir := writeManifestWithServices(t, []Service{{Name: "vllm", Type: "docker", ComposeFile: "services/vllm.yml"}})
	t.Setenv("PATH", t.TempDir())
	probed := false
	started := startManagedServicesForHost(context.Background(), func() int { return 0 }, func(string) bool { t.Fatal("native probe"); return false }, func(string, string) error { t.Fatal("native launch"); return nil }, func(name string) (string, bool) {
		probed = true
		return "Adopted external vllm", true
	})
	if !probed || len(started) != 1 || started[0].name != "vllm" {
		t.Fatalf("adoption=%v started=%v", probed, started)
	}
	if _, err := os.Stat(filepath.Join(dir, "services/vllm.yml")); !os.IsNotExist(err) {
		t.Fatal("boot adoption created compose", err)
	}
}

func TestControlCenterCUDAListUsesServingObservation(t *testing.T) {
	writeManifestWithServices(t, nil)
	t.Setenv("CITADEL_NODE_DIR", "")
	probed := false
	names := controlCenterAvailableServicesForHost("linux", func() int { return 0 }, func(_ context.Context, name string, port int) ([]string, bool) {
		probed = true
		if name != "vllm" || port <= 0 {
			t.Fatalf("unexpected observation: %s %d", name, port)
		}
		return []string{"synthetic-model"}, true
	})
	if !probed || !slices.Contains(names, "vllm") {
		t.Fatalf("serving engine omitted: %v, probed=%v", names, probed)
	}
	for _, name := range []string{"bonsai", "sglang", "diffusers"} {
		if slices.Contains(names, name) {
			t.Fatal("non-adoptable engine included", name)
		}
	}
}

func TestCLIStartCUDAAbsenceAdoptionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name         string
		service      string
		isolated     bool
		bind         string
		corruptOwner bool
		wantProbe    bool
	}{
		{name: "not serving", service: "vllm", wantProbe: true},
		{name: "not allowlisted", service: "bonsai"},
		{name: "isolated", service: "vllm", isolated: true},
		{name: "invalid bind", service: "vllm", bind: "invalid"},
		{name: "invalid ownership", service: "vllm", corruptOwner: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CITADEL_NODE_DIR", "")
			dir := writeManifestWithServices(t, []Service{{Name: tc.service, Type: "docker", Bind: tc.bind}})
			t.Setenv("PATH", t.TempDir())
			if tc.isolated {
				t.Setenv("CITADEL_NODE_DIR", dir)
			}
			if tc.corruptOwner {
				if err := os.WriteFile(externalengine.Path(network.GetNodeConfigDir()), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			probed := false
			err := startServiceForHost(tc.service, filepath.Join(dir, "services", tc.service+".yml"), func() int { return 0 }, func(string) (string, bool) {
				probed = true
				return "", false
			})
			if err == nil || probed != tc.wantProbe {
				t.Fatalf("err=%v probed=%v wantProbe=%v", err, probed, tc.wantProbe)
			}
			if _, err := os.Stat(filepath.Join(dir, "services", tc.service+".yml")); !os.IsNotExist(err) {
				t.Fatal("refused launch created compose", err)
			}
		})
	}
}

func TestControlCenterCUDAListObservationBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name                                      string
		gpu                                       int
		isolated, serving, wantProbe, wantVisible bool
	}{
		{name: "not serving", gpu: 0, wantProbe: true},
		{name: "isolated", gpu: 0, isolated: true, serving: true},
		{name: "unknown", gpu: -1, wantVisible: true},
		{name: "present", gpu: 1, wantVisible: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeManifestWithServices(t, nil)
			t.Setenv("CITADEL_NODE_DIR", "")
			if tc.isolated {
				t.Setenv("CITADEL_NODE_DIR", dir)
			}
			probed, samples := false, 0
			names := controlCenterAvailableServicesForHost("linux", func() int { samples++; return tc.gpu }, func(context.Context, string, int) ([]string, bool) { probed = true; return nil, tc.serving })
			if probed != tc.wantProbe || slices.Contains(names, "vllm") != tc.wantVisible || samples != 1 {
				t.Fatalf("probed=%v names=%v samples=%d", probed, names, samples)
			}
		})
	}
}

func TestCLIStartCUDAAbsenceFailsBeforeCompose(t *testing.T) {
	writeManifestWithServices(t, nil)
	t.Setenv("PATH", t.TempDir())
	for _, name := range []string{"bonsai", "unlimited-ocr", "vllm", "sglang", "diffusers", "omnivoice"} {
		if err := startService(name, "", func() int { return 0 }); err == nil || !strings.Contains(err.Error(), "requires an NVIDIA GPU, not starting") {
			t.Fatal("no early CUDA refusal", name, err)
		}
	}
	for _, count := range []int{-1, 1} {
		if err := startService("bonsai", "", func() int { return count }); err == nil || !strings.Contains(err.Error(), "has no compose_file") {
			t.Fatal("unknown/present probe suppressed normal start", count, err)
		}
	}
}

func TestCLIExplicitRunCUDARefusalPreservesDurableState(t *testing.T) {
	for _, name := range []string{"bonsai", "unlimited-ocr", "vllm", "sglang", "diffusers", "omnivoice"} {
		for _, state := range []string{"fresh", "unregistered", "stopped"} {
			t.Run(name+"/"+state, func(t *testing.T) {
				t.Setenv("CITADEL_NODE_DIR", "")
				var configDir string
				if state == "fresh" {
					home := t.TempDir()
					useCmdFixtureHome(t, home)
					configDir = filepath.Join(home, "citadel-node")
				} else {
					var configured []Service
					if state == "stopped" {
						configured = []Service{{Name: name, Type: "docker", ComposeFile: "services/" + name + ".yml", DesiredStatus: "stopped"}}
					}
					configDir = writeManifestWithServices(t, configured)
				}
				composePath := filepath.Join(configDir, "services", name+".yml")
				if state == "stopped" {
					if err := os.WriteFile(composePath, []byte("# keep the existing compose unchanged\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				paths := []string{filepath.Join(configDir, "citadel.yaml"), composePath, filepath.Join(os.Getenv("HOME"), ".citadel-cli", "config.yaml")}
				before := make(map[string][]byte)
				for _, path := range paths {
					data, err := os.ReadFile(path)
					if err != nil && !os.IsNotExist(err) {
						t.Fatal(err)
					}
					if err == nil {
						before[path] = data
					}
				}
				t.Setenv("PATH", t.TempDir())
				resolved, err := localServiceConfigDir()
				if err != nil || resolved != configDir {
					t.Fatalf("resolve run target: %q %v", resolved, err)
				}
				started, probes := false, 0
				err = runSingleServiceApply(resolved, name, func(serviceName, path string) error {
					started = true
					return startService(serviceName, path, func() int { return 0 })
				}, func() int { probes++; return 0 })
				if err == nil || !strings.Contains(err.Error(), "requires an NVIDIA GPU, not starting") || started || probes != 1 {
					t.Errorf("refused run: err=%v started=%t probes=%d", err, started, probes)
				}
				for _, path := range paths {
					got, err := os.ReadFile(path)
					want, existed := before[path]
					if existed {
						if err != nil || string(got) != string(want) {
							t.Errorf("refusal mutated %s (%v)", filepath.Base(path), err)
						}
					} else if !os.IsNotExist(err) {
						t.Errorf("refusal created %s (%v)", filepath.Base(path), err)
					}
				}
			})
		}
	}
}

func TestCLIExplicitRunUnknownOrPresentGPUKeepsStart(t *testing.T) {
	for _, count := range []int{-1, 1} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			t.Setenv("CITADEL_NODE_DIR", "")
			configDir := writeManifestWithServices(t, nil)
			t.Setenv("PATH", t.TempDir())
			started, probes := false, 0
			err := runSingleServiceApply(configDir, "bonsai", func(string, string) error { started = true; return nil }, func() int { probes++; return count })
			if err != nil || !started || probes != 1 {
				t.Fatalf("normal run: err=%v started=%t probes=%d", err, started, probes)
			}
		})
	}
}

func TestCLIExplicitRunCPUServiceDoesNotProbeGPU(t *testing.T) {
	t.Setenv("CITADEL_NODE_DIR", "")
	configDir := writeManifestWithServices(t, []Service{{Name: "kokoro", Type: "docker", ComposeFile: "services/kokoro.yml", DesiredStatus: "stopped"}})
	t.Setenv("PATH", t.TempDir())
	started := false
	err := runSingleServiceApply(configDir, "kokoro", func(string, string) error { started = true; return nil }, func() int {
		t.Fatal("CPU-capable service probed for CUDA admission")
		return 0
	})
	if err != nil || !started {
		t.Fatalf("CPU start refused: err=%v started=%t", err, started)
	}
	manifest, _, err := findAndReadManifest()
	if err != nil || len(manifest.Services) != 1 || manifest.Services[0].DesiredStatus != "" {
		t.Fatalf("explicit CPU start did not clear stopped marker: %v", err)
	}
}

func TestControlCenterConfiguredCUDARefusesBeforeEffects(t *testing.T) {
	for action, change := range map[string]func(string, func() int) error{"start": ccStartServiceForHost, "restart": ccRestartServiceForHost} {
		for _, name := range []string{"bonsai", "unlimited-ocr", "vllm", "sglang", "diffusers", "omnivoice"} {
			t.Run(action+"/"+name, func(t *testing.T) {
				dir := writeManifestWithServices(t, []Service{{Name: name, Type: "docker", ComposeFile: "services/" + name + ".yml", DesiredStatus: "stopped"}})
				t.Setenv("PATH", t.TempDir())
				before, err := os.ReadFile(filepath.Join(dir, "citadel.yaml"))
				if err != nil {
					t.Fatal(err)
				}
				probes := 0
				err = change(name, func() int { probes++; return 0 })
				if err == nil || !strings.Contains(err.Error(), "requires an NVIDIA GPU, not starting") || probes != 1 {
					t.Fatalf("missing host refusal: %v, probes=%d", err, probes)
				}
				after, err := os.ReadFile(filepath.Join(dir, "citadel.yaml"))
				if err != nil || string(after) != string(before) {
					t.Fatal("refusal changed stopped manifest", err)
				}
			})
		}
	}
}

func TestControlCenterConfiguredCPUReconcilesBeforeRuntimeEffects(t *testing.T) {
	t.Setenv("CITADEL_NODE_DIR", "")
	for action, change := range map[string]func(string, func() int) error{"start": ccStartServiceForHost, "restart": ccRestartServiceForHost} {
		for _, name := range []string{"ollama", "llamacpp"} {
			t.Run(action+"/"+name, func(t *testing.T) {
				dir := writeManifestWithServices(t, []Service{{Name: name, Type: "docker", ComposeFile: "services/" + name + ".yml", DesiredStatus: "stopped"}})
				if err := os.MkdirAll(filepath.Join(dir, "services"), 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "services", name+".yml")
				if err := os.WriteFile(path, []byte(services.ServiceMap[name]), 0600); err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 37\n"), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin)
				t.Setenv("CITADEL_CONTAINER_RUNTIME", "docker")
				probes := 0
				if err := change(name, func() int { probes++; return 0 }); err == nil {
					t.Fatal("fake engine did not refuse")
				}
				expected, _ := services.ComposeForHost(name, "linux", func() int { return 0 })
				got, err := os.ReadFile(path)
				if err != nil || string(got) != expected || probes != 1 {
					t.Fatalf("configured service kept GPU compose: %v, probes=%d", err, probes)
				}
			})
		}
	}
}

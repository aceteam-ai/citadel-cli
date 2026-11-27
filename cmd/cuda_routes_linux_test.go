//go:build linux

package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/externalengine"
	"github.com/aceteam-ai/citadel-cli/internal/network"
)

func writeCUDAAdoptionRecord(t *testing.T, mode string) externalengine.Config {
	t.Helper()
	c, err := externalengine.Validate(externalengine.Config{
		Version: 1, Mode: mode,
		Endpoint: externalengine.Endpoint{Host: "127.0.0.1", Port: 58001},
		Model:    "synthetic/model", Revision: "1",
		RequestID: "00000000-0000-4000-8000-000000000001", NodeID: "1",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := externalengine.Save(network.GetNodeConfigDir(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

// Exercise the guarded CLI and TUI routes, not merely their shared admission
// helper. No start callback, compose publication or native process may run.
func TestCUDAActualStartRoutesAdoption(t *testing.T) {
	routes := map[string]func(string, func() int, func(string) (string, bool)) error{
		"cli": func(dir string, gpu func() int, adopt func(string) (string, bool)) error {
			return runSingleServiceApplyForHost(dir, "vllm", func(string, string) error {
				t.Fatal("managed start callback reached")
				return nil
			}, gpu, adopt)
		},
		"control center start": func(_ string, gpu func() int, adopt func(string) (string, bool)) error {
			return ccStartServiceForHostWithAdoption("vllm", gpu, adopt)
		},
		"control center restart": func(_ string, gpu func() int, adopt func(string) (string, bool)) error {
			return ccRestartServiceForHostWithAdoption("vllm", gpu, adopt)
		},
	}
	for route, run := range routes {
		for _, tc := range []struct {
			name, record                           string
			gpu                                    int
			live, isolated, wantProbe, wantSuccess bool
		}{
			{name: "live unrecorded", gpu: 0, live: true, wantProbe: true, wantSuccess: true},
			{name: "unrecorded unavailable", gpu: 0, wantProbe: true},
			{name: "live adopted", record: "adopted", gpu: 0, live: true, wantProbe: true, wantSuccess: true},
			{name: "adopted unknown hardware", record: "adopted", gpu: -1, live: true, wantProbe: true, wantSuccess: true},
			{name: "adopted present hardware", record: "adopted", gpu: 1, live: true, wantProbe: true, wantSuccess: true},
			{name: "adopted unavailable", record: "adopted", gpu: 0, wantProbe: true},
			{name: "adopted unavailable with GPU", record: "adopted", gpu: 1, wantProbe: true},
			{name: "detached", record: "detached", gpu: 0, live: true},
			{name: "malformed", record: "malformed", gpu: 0, live: true},
			{name: "isolated", gpu: 0, live: true, isolated: true},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				t.Setenv("CITADEL_NODE_DIR", "")
				dir := writeManifestWithServices(t, []Service{{Name: "vllm", Type: "docker", ComposeFile: "services/vllm.yml", DesiredStatus: "stopped"}})
				t.Setenv("PATH", t.TempDir())
				if tc.isolated {
					t.Setenv("CITADEL_NODE_DIR", dir)
				}
				if tc.record == "malformed" {
					if err := os.WriteFile(externalengine.Path(network.GetNodeConfigDir()), []byte("{"), 0600); err != nil {
						t.Fatal(err)
					}
				} else if tc.record != "" {
					writeCUDAAdoptionRecord(t, tc.record)
				}
				probed, samples := false, 0
				err := run(dir, func() int { samples++; return tc.gpu }, func(name string) (string, bool) {
					if name != "vllm" {
						t.Fatal(name)
					}
					probed = true
					return "synthetic external engine", tc.live
				})
				if (err == nil) != tc.wantSuccess || probed != tc.wantProbe || samples != 1 {
					t.Fatalf("err=%v probe=%v samples=%d; want success=%v probe=%v", err, probed, samples, tc.wantSuccess, tc.wantProbe)
				}
				manifest, err := readManifestAt(dir)
				if err != nil || len(manifest.Services) != 1 {
					t.Fatalf("manifest: %v", err)
				}
				wantStatus := "stopped"
				if tc.wantSuccess {
					wantStatus = ""
				}
				if manifest.Services[0].DesiredStatus != wantStatus {
					t.Fatalf("desired status=%q want %q", manifest.Services[0].DesiredStatus, wantStatus)
				}
				if _, err := os.Stat(filepath.Join(dir, "services", "vllm.yml")); !os.IsNotExist(err) {
					t.Fatal("admission materialized compose", err)
				}
			})
		}
	}
}

func TestCUDAAdoptedRecordUsesExactFreshProbe(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprint(live), func(t *testing.T) {
			t.Setenv("CITADEL_NODE_DIR", "")
			writeManifestWithServices(t, nil)
			t.Setenv("PATH", t.TempDir())
			c := writeCUDAAdoptionRecord(t, "adopted")
			calls := 0
			observe := func(_ context.Context, endpoint externalengine.Endpoint, model string) error {
				calls++
				if endpoint != c.Endpoint || model != c.Model {
					t.Fatalf("probe did not bind exact record: %v %q", endpoint, model)
				}
				if !live {
					return fmt.Errorf("synthetic unavailable model")
				}
				return nil
			}
			for range 2 {
				_, adopted := adoptExternalEngineForStartWithProbe("vllm", observe)
				if adopted != live {
					t.Fatalf("adopted=%v want %v", adopted, live)
				}
			}
			if calls != 2 {
				t.Fatalf("probe was cached: calls=%d", calls)
			}
			writeCUDAAdoptionRecord(t, "detached")
			if _, adopted := adoptExternalEngineForStartWithProbe("vllm", observe); adopted || calls != 2 {
				t.Fatal("detached record observed or adopted")
			}
		})
	}
}

func TestCUDAExplicitRunAdoptsWithoutCreatingManagedState(t *testing.T) {
	for _, state := range []string{"fresh", "unregistered"} {
		t.Run(state, func(t *testing.T) {
			t.Setenv("CITADEL_NODE_DIR", "")
			var dir string
			if state == "fresh" {
				useCmdFixtureHome(t, t.TempDir())
				var err error
				dir, err = localServiceConfigDir()
				if err != nil {
					t.Fatal(err)
				}
			} else {
				dir = writeManifestWithServices(t, nil)
			}
			t.Setenv("PATH", t.TempDir())
			before, beforeErr := os.ReadFile(filepath.Join(dir, "citadel.yaml"))
			err := runSingleServiceApplyForHost(dir, "vllm", func(string, string) error { t.Fatal("managed start reached"); return nil }, func() int { return 0 }, func(string) (string, bool) { return "synthetic serving engine", true })
			if err != nil {
				t.Fatal(err)
			}
			after, afterErr := os.ReadFile(filepath.Join(dir, "citadel.yaml"))
			if beforeErr == nil {
				if afterErr != nil || string(after) != string(before) {
					t.Fatal("adoption changed unregistered manifest", afterErr)
				}
			} else if !os.IsNotExist(afterErr) {
				t.Fatal("adoption created managed manifest", afterErr)
			}
			if _, err := os.Stat(filepath.Join(dir, "services", "vllm.yml")); !os.IsNotExist(err) {
				t.Fatal("adoption created compose", err)
			}
		})
	}
}

func TestInitialServiceChoicesRespectHostAndServingEvidence(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		gpu                           int
		live, wantVLLM, wantDiffusers bool
	}{
		{name: "absent", gpu: 0},
		{name: "absent with serving engine", gpu: 0, live: true, wantVLLM: true},
		{name: "unknown", gpu: -1, wantVLLM: true, wantDiffusers: true},
		{name: "present", gpu: 1, wantVLLM: true, wantDiffusers: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CITADEL_NODE_DIR", "")
			writeManifestWithServices(t, nil)
			options := initialServiceOptionsForHost("linux", func() int { return tc.gpu }, func(context.Context, string, int) ([]string, bool) { return nil, tc.live })
			var names []string
			for _, option := range options {
				names = append(names, strings.Fields(option)[0])
			}
			if slices.Contains(names, "vllm") != tc.wantVLLM || slices.Contains(names, "diffusers") != tc.wantDiffusers {
				t.Fatal(names)
			}
			for _, name := range []string{"none", "ollama", "llamacpp", "extraction"} {
				if !slices.Contains(names, name) {
					t.Fatal("missing CPU/network choice", name, names)
				}
			}
		})
	}
}

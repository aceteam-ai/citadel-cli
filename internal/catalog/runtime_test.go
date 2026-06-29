package catalog

import (
	"reflect"
	"testing"
)

// probeStub builds a runtimeProbes whose behavior is fully driven by the test,
// so the selection policy is exercised without podman/docker installed.
func probeStub(present map[string]bool, composeSubcmd bool) runtimeProbes {
	return runtimeProbes{
		lookPath:            func(bin string) bool { return present[bin] },
		podmanComposeSubcmd: func() bool { return composeSubcmd },
	}
}

func TestSelectContainerRuntime(t *testing.T) {
	tests := []struct {
		name          string
		present       map[string]bool
		composeSubcmd bool
		wantEngineBin string
		wantBin       string
		wantPrefix    []string
		wantRootless  bool
	}{
		{
			name:          "podman with compose subcommand preferred",
			present:       map[string]bool{"podman": true, "docker": true},
			composeSubcmd: true,
			wantEngineBin: "podman",
			wantBin:       "podman",
			wantPrefix:    []string{"compose"},
			wantRootless:  true,
		},
		{
			name:          "podman without subcommand falls back to podman-compose binary",
			present:       map[string]bool{"podman": true, "podman-compose": true, "docker": true},
			composeSubcmd: false,
			// Engine sub-commands (inspect/rm) must target podman, NOT the
			// podman-compose wrapper (which has no inspect/rm).
			wantEngineBin: "podman",
			wantBin:       "podman-compose",
			wantPrefix:    nil,
			wantRootless:  true,
		},
		{
			name:          "podman present but no compose front-end falls back to docker",
			present:       map[string]bool{"podman": true, "docker": true},
			composeSubcmd: false,
			wantEngineBin: "docker",
			wantBin:       "docker",
			wantPrefix:    []string{"compose"},
			wantRootless:  false,
		},
		{
			name:          "podman absent uses docker",
			present:       map[string]bool{"docker": true},
			composeSubcmd: false,
			wantEngineBin: "docker",
			wantBin:       "docker",
			wantPrefix:    []string{"compose"},
			wantRootless:  false,
		},
		{
			name:          "neither present still returns docker (selection never fails)",
			present:       map[string]bool{},
			composeSubcmd: false,
			wantEngineBin: "docker",
			wantBin:       "docker",
			wantPrefix:    []string{"compose"},
			wantRootless:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := selectContainerRuntime(probeStub(tt.present, tt.composeSubcmd))
			if rt.EngineBin != tt.wantEngineBin {
				t.Errorf("EngineBin = %q, want %q", rt.EngineBin, tt.wantEngineBin)
			}
			if rt.Bin != tt.wantBin {
				t.Errorf("Bin = %q, want %q", rt.Bin, tt.wantBin)
			}
			if !reflect.DeepEqual(rt.ComposePrefix, tt.wantPrefix) {
				t.Errorf("ComposePrefix = %v, want %v", rt.ComposePrefix, tt.wantPrefix)
			}
			if rt.Rootless != tt.wantRootless {
				t.Errorf("Rootless = %v, want %v", rt.Rootless, tt.wantRootless)
			}
			// EngineBin is never the compose wrapper.
			if rt.EngineBin == "podman-compose" {
				t.Errorf("EngineBin must never be the podman-compose wrapper")
			}
		})
	}
}

func TestContainerRuntime_ComposeArgs(t *testing.T) {
	docker := ContainerRuntime{Bin: "docker", ComposePrefix: []string{"compose"}}
	got := docker.ComposeArgs("-f", "x.yml", "up", "-d")
	want := []string{"compose", "-f", "x.yml", "up", "-d"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("docker ComposeArgs = %v, want %v", got, want)
	}

	// podman-compose has no prefix: the args pass through unchanged.
	pc := ContainerRuntime{Bin: "podman-compose", ComposePrefix: nil}
	got = pc.ComposeArgs("-f", "x.yml", "up", "-d")
	want = []string{"-f", "x.yml", "up", "-d"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("podman-compose ComposeArgs = %v, want %v", got, want)
	}
}

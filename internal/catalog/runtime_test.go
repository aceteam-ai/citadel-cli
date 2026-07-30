package catalog

import (
	"reflect"
	"strings"
	"testing"
)

// probeStub builds a runtimeProbes whose behavior is fully driven by the test,
// so the selection policy is exercised without podman/docker installed.
func probeStub(present map[string]bool, composeSubcmd bool) runtimeProbes {
	// A live socket and no override keep these cases exercising the pre-#636
	// policy unchanged; socket-dead and override behavior are covered below.
	return probeStubFull(present, composeSubcmd, true, "")
}

// probeStubFull additionally drives the podman API-socket liveness probe and the
// explicit runtime override (#636).
func probeStubFull(present map[string]bool, composeSubcmd, socketLive bool, override string) runtimeProbes {
	return runtimeProbes{
		lookPath:            func(bin string) bool { return present[bin] },
		podmanComposeSubcmd: func() bool { return composeSubcmd },
		podmanSocketLive:    func() bool { return socketLive },
		override:            func() string { return override },
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

// TestPodmanComposeSubcmdIsNotLiveness pins the #636 root cause: `podman compose
// version` succeeds by delegating to an external provider (usually
// docker-compose) that is merely present on PATH, so it says nothing about
// whether podman's API socket is listening. Treating it as liveness selected a
// runtime whose every command then failed with "Cannot connect to the Docker
// daemon at unix:///run/user/1000/podman/podman.sock".
func TestPodmanComposeSubcmdIsNotLiveness(t *testing.T) {
	p := probeStubFull(map[string]bool{"podman": true, "docker": true}, true, false, "")
	got := selectContainerRuntime(p)
	if got.EngineBin != "docker" {
		t.Fatalf("EngineBin = %q, want docker when podman's socket is dead", got.EngineBin)
	}
	if got.FallbackReason == "" {
		t.Error("want a FallbackReason explaining the downgrade, got empty")
	}
}

// A dead socket must not abandon podman when the podman-compose wrapper is
// available: that wrapper drives the podman CLI directly and needs no socket.
func TestPodmanSocketDeadPrefersWrapperOverDocker(t *testing.T) {
	p := probeStubFull(map[string]bool{"podman": true, "podman-compose": true, "docker": true}, true, false, "")
	got := selectContainerRuntime(p)
	if got.EngineBin != "podman" || got.Bin != "podman-compose" {
		t.Fatalf("got EngineBin=%q Bin=%q, want podman/podman-compose", got.EngineBin, got.Bin)
	}
	if got.FallbackReason != "" {
		t.Errorf("the wrapper is a first-class front-end, not a downgrade; got reason %q", got.FallbackReason)
	}
}

func TestRuntimeOverride(t *testing.T) {
	full := map[string]bool{"podman": true, "docker": true}

	// docker forced even though podman is fully usable.
	if got := selectContainerRuntime(probeStubFull(full, true, true, "docker")); got.EngineBin != "docker" {
		t.Errorf("override=docker: EngineBin = %q, want docker", got.EngineBin)
	}

	// podman forced even though its socket is dead: fail loudly ON PODMAN rather
	// than silently running docker behind the operator's back.
	if got := selectContainerRuntime(probeStubFull(full, true, false, "podman")); got.EngineBin != "podman" {
		t.Errorf("override=podman: EngineBin = %q, want podman (forced)", got.EngineBin)
	}

	// Case and surrounding whitespace are tolerated.
	if got := selectContainerRuntime(probeStubFull(full, true, true, "  DOCKER  ")); got.EngineBin != "docker" {
		t.Errorf("override=\"  DOCKER  \": EngineBin = %q, want docker", got.EngineBin)
	}

	// An unrecognized value must not silently pin a runtime: auto-detect, but
	// tell the operator their setting was ignored.
	got := selectContainerRuntime(probeStubFull(full, true, true, "containerd"))
	if got.EngineBin != "podman" {
		t.Errorf("bad override: EngineBin = %q, want auto-detected podman", got.EngineBin)
	}
	if !strings.Contains(got.FallbackReason, "unrecognized") {
		t.Errorf("bad override: FallbackReason = %q, want it to mention the ignored value", got.FallbackReason)
	}
}

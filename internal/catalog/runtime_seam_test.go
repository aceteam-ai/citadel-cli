package catalog

import (
	"context"
	"strings"
	"testing"
)

// These pin the ContainerRuntime exec-seam's argv contract directly — the one
// behavioral edge citadel-cli#1041's refactor introduced. The many converted
// call sites are only byte-identical to their prior exec.Command("docker", ...)
// forms if this holds, and nothing else tests the podman-compose-wrapper case
// (where ComposePrefix is empty and Bin is "podman-compose").

func argsOf(args []string) string { return strings.Join(args, " ") }

func TestEngineCommand_DockerArgv(t *testing.T) {
	rt := ContainerRuntime{EngineBin: "docker", Bin: "docker", ComposePrefix: []string{"compose"}}
	got := rt.EngineCommand("inspect", "--format", "{{.State.Status}}", "citadel-vllm").Args
	want := []string{"docker", "inspect", "--format", "{{.State.Status}}", "citadel-vllm"}
	if argsOf(got) != argsOf(want) {
		t.Fatalf("EngineCommand().Args = %v, want %v", got, want)
	}
}

func TestEngineCommand_PodmanArgv(t *testing.T) {
	rt := ContainerRuntime{EngineBin: "podman", Bin: "podman", ComposePrefix: []string{"compose"}}
	got := rt.EngineCommand("ps", "--format", "{{.Names}}").Args
	want := []string{"podman", "ps", "--format", "{{.Names}}"}
	if argsOf(got) != argsOf(want) {
		t.Fatalf("EngineCommand().Args = %v, want %v", got, want)
	}
}

func TestEngineCommand_ZeroValueDefaultsToDocker(t *testing.T) {
	// SelectContainerRuntime never returns an empty EngineBin, but a zero-value
	// ContainerRuntime must still exec "docker" (the pre-seam default at every
	// converted call site).
	got := ContainerRuntime{}.EngineCommand("info").Args
	if argsOf(got) != "docker info" {
		t.Fatalf("zero-value EngineCommand().Args = %v, want [docker info]", got)
	}
}

func TestComposeCommand_DockerPrependsCompose(t *testing.T) {
	rt := ContainerRuntime{EngineBin: "docker", Bin: "docker", ComposePrefix: []string{"compose"}}
	got := rt.ComposeCommand("-p", "x", "-f", "/n/vllm.yml", "up", "-d").Args
	want := []string{"docker", "compose", "-p", "x", "-f", "/n/vllm.yml", "up", "-d"}
	if argsOf(got) != argsOf(want) {
		t.Fatalf("ComposeCommand().Args = %v, want %v (docker compose ...)", got, want)
	}
}

func TestComposeCommand_PodmanSubcommandPrependsCompose(t *testing.T) {
	rt := ContainerRuntime{EngineBin: "podman", Bin: "podman", ComposePrefix: []string{"compose"}, Rootless: true}
	got := rt.ComposeCommand("-f", "/n/vllm.yml", "down").Args
	want := []string{"podman", "compose", "-f", "/n/vllm.yml", "down"}
	if argsOf(got) != argsOf(want) {
		t.Fatalf("ComposeCommand().Args = %v, want %v (podman compose ...)", got, want)
	}
}

func TestComposeCommand_PodmanComposeWrapperOmitsPrefix(t *testing.T) {
	// The separate podman-compose binary IS the compose front-end, so there is no
	// "compose" subcommand to prepend. A converted site that blindly re-added
	// "compose" (or used EngineCommand for a compose call) would break here.
	rt := ContainerRuntime{EngineBin: "podman", Bin: "podman-compose", ComposePrefix: nil, Rootless: true}
	got := rt.ComposeCommand("-f", "/n/vllm.yml", "up", "-d").Args
	want := []string{"podman-compose", "-f", "/n/vllm.yml", "up", "-d"}
	if argsOf(got) != argsOf(want) {
		t.Fatalf("ComposeCommand().Args = %v, want %v (podman-compose, no compose prefix)", got, want)
	}
}

func TestCommandContext_CarriesCancellation(t *testing.T) {
	// EngineCommandContext / ComposeCommandContext must bind the context (a site
	// that used exec.CommandContext must keep kill-on-cancel). A cancelled ctx
	// makes Start fail, which the non-context EngineCommand would not.
	rt := ContainerRuntime{EngineBin: "docker", Bin: "docker", ComposePrefix: []string{"compose"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rt.EngineCommandContext(ctx, "info").Start(); err == nil {
		t.Fatal("EngineCommandContext with a cancelled ctx: Start() = nil, want a cancellation error")
	}
	if err := rt.ComposeCommandContext(ctx, "version").Start(); err == nil {
		t.Fatal("ComposeCommandContext with a cancelled ctx: Start() = nil, want a cancellation error")
	}
}

func TestGPUArgs(t *testing.T) {
	tests := []struct {
		name string
		rt   ContainerRuntime
		spec string
		want []string
	}{
		{"docker legacy syntax", ContainerRuntime{EngineBin: "docker"}, "device=2,3", []string{"--gpus", "device=2,3"}},
		{"podman all CDI", ContainerRuntime{EngineBin: "podman"}, "all", []string{"--device", "nvidia.com/gpu=all"}},
		{"podman explicit CDI", ContainerRuntime{EngineBin: "podman"}, "device=2,3", []string{"--device", "nvidia.com/gpu=2", "--device", "nvidia.com/gpu=3"}},
		{"podman count CDI", ContainerRuntime{EngineBin: "podman"}, "2", []string{"--device", "nvidia.com/gpu=0", "--device", "nvidia.com/gpu=1"}},
		{"empty", ContainerRuntime{EngineBin: "podman"}, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.rt.GPUArgs(tt.spec)
			if err != nil {
				t.Fatalf("GPUArgs(%q): %v", tt.spec, err)
			}
			if argsOf(got) != argsOf(tt.want) {
				t.Fatalf("GPUArgs(%q) = %v, want %v", tt.spec, got, tt.want)
			}
		})
	}
}

func TestGPUArgsPodmanRejectsAmbiguousSelector(t *testing.T) {
	_, err := (ContainerRuntime{EngineBin: "podman"}).GPUArgs("device=0;--privileged")
	if err == nil {
		t.Fatal("GPUArgs() error = nil, want unsupported-selector error")
	}
}

package cmd

import (
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
)

// TestStopServiceByComposeArgvByteIdentical pins that stopServiceByCompose's
// citadel-cli#1041 conversion is byte-identical to the prior
// exec.Command("docker", stopComposeArgs(...)...) on a docker node: it strips
// the leading "compose" from stopComposeArgs' output and lets rt.ComposeCommand
// re-add the runtime's own front-end prefix. stopComposeArgs itself is pinned by
// TestStopComposeArgs* (nodedir_test.go); this pins the exec reconstruction the
// [1:] strip depends on.
func TestStopServiceByComposeArgvByteIdentical(t *testing.T) {
	setNodeDirOverrideForTest(t, "")
	rt := catalog.ContainerRuntime{EngineBin: "docker", Bin: "docker", ComposePrefix: []string{"compose"}}

	full := stopComposeArgs("/n/services/vllm.yml", false)
	if len(full) == 0 || full[0] != "compose" {
		t.Fatalf("stopComposeArgs() = %v, want it to start with \"compose\" (the [1:] strip depends on this)", full)
	}
	got := rt.ComposeCommand(full[1:]...).Args
	want := []string{"docker", "compose", "-f", "/n/services/vllm.yml", "down"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("reconstructed stop argv = %v, want %v (byte-identical to prior docker compose down)", got, want)
	}
}

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/compose"
)

// livePS is a real `docker compose -f vllm.yml ps --format json` capture from a
// citadel node, trimmed and genericized. It contains nine RUNNING containers and
// no vllm: `ps` is project-scoped (citadel passes no `-p`, #528) and every
// service compose file shares the default project, so a caller that reads the
// first record out of it reports every declared service as running (#692).
const livePS = `{"ID":"61bfa011e53f","Name":"citadel-bonsai","Service":"bonsai","State":"running","Status":"Up 10 hours"}
{"ID":"53b6c466f956","Name":"citadel-kokoro","Service":"kokoro","State":"running","Status":"Up 10 hours (healthy)"}
{"ID":"94b5ccaf96eb","Name":"citadel-tei","Service":"tei","State":"running","Status":"Up 10 hours"}
{"ID":"2b1f20435fb5","Name":"services-bridge-1","Service":"bridge","State":"running","Status":"Up 10 hours (healthy)"}
{"ID":"cd3c5f192d7e","Name":"services-db-1","Service":"db","State":"running","Status":"Up 10 hours (healthy)"}`

// writeCompose writes a minimal compose file declaring the given service keys
// and returns its path.
func writeCompose(t *testing.T, name string, serviceKeys ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("services:\n")
	for _, k := range serviceKeys {
		b.WriteString("  " + k + ":\n    image: example/" + k + ":latest\n")
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write compose: %v", err)
	}
	return path
}

// TestComposeServiceStateFromContainerBacked: the service's own container is in
// the project-wide output and is up.
func TestComposeServiceStateFromContainerBacked(t *testing.T) {
	path := writeCompose(t, "kokoro.yml", "kokoro")
	got := composeServiceStateFrom([]byte(livePS), path, "kokoro")
	if !got.Running || got.State != compose.StateRunning {
		t.Fatalf("kokoro: got %+v, want running", got)
	}
	if got.Container == nil || got.Container.Name != "citadel-kokoro" {
		t.Fatalf("resolved to the wrong container: %+v", got.Container)
	}
}

// TestComposeServiceStateFromAbsent is the #692 regression test at the CLI
// boundary: the service has no container, yet the ps output it is resolved
// against is full of other services' running containers.
//
// The service name is one with no native equivalent (internal/services.
// NativeServices covers only ollama/llamacpp/vllm), so the native probe returns
// false without opening a socket and the assertion does not depend on what
// happens to be listening on the test machine. The container-vs-native ordering
// itself is tested deterministically with an injected probe in
// internal/compose.TestResolveServiceStateNative.
func TestComposeServiceStateFromAbsent(t *testing.T) {
	path := writeCompose(t, "diffusers.yml", "diffusers")
	got := composeServiceStateFrom([]byte(livePS), path, "diffusers")
	if got.Running || got.State != compose.StateStopped {
		t.Fatalf("absent service: got %+v, want stopped (#692)", got)
	}
	if got.Container != nil {
		t.Errorf("absent service must not claim a container: %+v", got.Container)
	}
}

// TestComposeServiceStateFromMultiContainer: whatsapp-bridge declares `bridge`
// and `db` and pins no container_name, so its containers are services-bridge-1 /
// services-db-1. A "citadel-<name>" container lookup would miss them and report
// a running service as stopped.
func TestComposeServiceStateFromMultiContainer(t *testing.T) {
	path := writeCompose(t, "whatsapp-bridge.yml", "bridge", "db")
	got := composeServiceStateFrom([]byte(livePS), path, "whatsapp-bridge")
	if !got.Running {
		t.Fatalf("whatsapp-bridge: got %+v, want running", got)
	}
}

// TestComposeServiceStateFromUnknownService: a service with no compose file on
// disk falls open to the previous project-wide reading rather than reporting a
// running service stopped.
func TestComposeServiceStateFromUnknownCompose(t *testing.T) {
	got := composeServiceStateFrom([]byte(livePS), "/nonexistent/compose.yml", "whatever")
	if !got.Running {
		t.Fatalf("unreadable compose must fail open, got %+v", got)
	}
}

// TestStatusNoProjectFlag pins the compose invocation convention on the status
// surface, mirroring internal/jobs.TestUpdateManifestNoProjectFlag. #692's
// suggested fix was to pass an explicit `-p` per service; doing that would
// reintroduce the project-name mismatch #528 removed (containers actually live
// in the shared default project) and status would then report every service
// stopped. The fix is to filter the project-wide ps output instead.
func TestStatusNoProjectFlag(t *testing.T) {
	for _, file := range []string{"status.go", "service.go", "controlcenter.go"} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Skipf("cannot read source %s: %v", file, err)
		}
		src := string(data)
		if strings.Contains(src, `"-p", projectName`) || strings.Contains(src, `"-p", "citadel-`) {
			t.Errorf("%s passes a compose -p project flag; the standardized convention is NO -p (#528)", file)
		}
	}
}

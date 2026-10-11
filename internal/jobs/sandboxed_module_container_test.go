//go:build integration

// Container-runtime proof for the generic sandboxed-module profile
// (citadel-cli#1288). Build-tagged `integration` so it NEVER runs in the
// hermetic unit suite (`go test ./...`); run it where a real runtime exists with
//
//	go test -tags=integration ./internal/jobs/ -run TestSandboxedProfile
//
// It renders the ACTUAL profile via services.RenderSandboxedModuleCompose,
// `compose up`s it through the catalog runtime seam, and proves the two security
// properties the issue requires -- the container cannot reach the network, and
// cannot write outside tmpfs -- plus that the host can reach the module at its
// static bridge IP, and that a SECOND, different module runs on the same profile
// with no runtime changes. Uses the small public python:3-alpine image; a
// container of exactly one test's resources, cleaned up via t.Cleanup.
package jobs

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/services"
)

const sandboxITImage = "docker.io/library/python:3-alpine"

func sandboxITServeSpec(name string, index, port int) services.SandboxedModuleSpec {
	return services.SandboxedModuleSpec{
		Name:          name,
		Image:         sandboxITImage,
		ContainerPort: port,
		NetworkIndex:  index,
		// Serve a trivial HTTP endpoint on all interfaces inside the container so
		// the host can reach it at the static bridge IP.
		Command: []string{"python3", "-m", "http.server", fmt.Sprint(port), "--bind", "0.0.0.0"},
	}
}

func dockerReachable(t *testing.T, rt catalog.ContainerRuntime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.Info(ctx).Run(); err != nil {
		t.Skipf("container runtime not reachable (%v); skipping sandbox profile integration test", err)
	}
}

// composeUp renders the profile for spec, writes it under t.TempDir(), and
// `compose up -d`s it, registering teardown. It returns the compose file path
// and project name.
func composeUp(t *testing.T, rt catalog.ContainerRuntime, spec services.SandboxedModuleSpec) (string, string) {
	t.Helper()
	yamlStr, err := services.RenderSandboxedModuleCompose(spec)
	if err != nil {
		t.Fatalf("render profile: %v", err)
	}
	dir := t.TempDir()
	file := dir + "/compose.yml"
	if err := os.WriteFile(file, []byte(yamlStr), 0o644); err != nil {
		t.Fatalf("write compose: %v", err)
	}
	project := "sbxit-" + spec.Name

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_ = rt.ComposeCommandContext(ctx, "-f", file, "-p", project, "down", "-v", "--remove-orphans").Run()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if out, err := rt.ComposeCommandContext(ctx, "-f", file, "-p", project, "up", "-d").CombinedOutput(); err != nil {
		t.Fatalf("compose up failed: %v\n%s", err, out)
	}
	return file, project
}

// waitReachable polls the static-IP endpoint from the HOST until it answers or
// the deadline passes. Proves the host-private endpoint is reachable.
func waitReachable(t *testing.T, spec services.SandboxedModuleSpec) {
	t.Helper()
	url := "http://" + services.SandboxedModuleEndpoint(spec) + "/"
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("host could not reach module at %s within timeout", url)
}

func execInContainer(t *testing.T, rt catalog.ContainerRuntime, name string, argv ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	args := append([]string{"exec", name}, argv...)
	return rt.EngineCommandContext(ctx, args...).CombinedOutput()
}

func TestSandboxedProfileEnforcesIsolation(t *testing.T) {
	rt := catalog.SelectContainerRuntime()
	dockerReachable(t, rt)

	spec := sandboxITServeSpec("sbxit-a", 200, 9099)
	composeUp(t, rt, spec)
	waitReachable(t, spec) // (endpoint reachable from the host)

	name := services.SandboxedModuleContainerName(spec.Name)

	// (1) NO outbound network: an egress TCP connect from inside the container
	// must fail (no default route on the internal network).
	out, _ := execInContainer(t, rt, name, "python3", "-c",
		"import socket;s=socket.socket();s.settimeout(4);print('connect_ex', s.connect_ex(('1.1.1.1',443)))")
	got := strings.TrimSpace(string(out))
	if !strings.Contains(got, "connect_ex") || strings.Contains(got, "connect_ex 0") {
		t.Errorf("egress to 1.1.1.1:443 should FAIL on the sandbox network; got %q", got)
	}

	// (2) read-only root filesystem: writing outside tmpfs must fail.
	if out, err := execInContainer(t, rt, name, "sh", "-c", "touch /should_fail"); err == nil {
		t.Errorf("touch / should fail on a read-only rootfs; got success, out=%q", out)
	}

	// (3) tmpfs scratch is writable.
	if out, err := execInContainer(t, rt, name, "sh", "-c", "touch /tmp/ok"); err != nil {
		t.Errorf("touch /tmp should succeed (tmpfs scratch); err=%v out=%q", err, out)
	}
}

// TestSandboxedProfileSecondModuleNoRuntimeChanges proves the acceptance that a
// SECOND, different helper module runs on the SAME profile with no runtime
// changes: a distinct name/index/port goes through the identical render+up path
// and is independently reachable and egress-blocked.
func TestSandboxedProfileSecondModuleNoRuntimeChanges(t *testing.T) {
	rt := catalog.SelectContainerRuntime()
	dockerReachable(t, rt)

	spec := sandboxITServeSpec("sbxit-b", 201, 9100)
	composeUp(t, rt, spec)
	waitReachable(t, spec)

	name := services.SandboxedModuleContainerName(spec.Name)
	out, _ := execInContainer(t, rt, name, "python3", "-c",
		"import socket;s=socket.socket();s.settimeout(4);print('connect_ex', s.connect_ex(('8.8.8.8',53)))")
	if got := strings.TrimSpace(string(out)); strings.Contains(got, "connect_ex 0") {
		t.Errorf("second module egress should FAIL; got %q", got)
	}
}

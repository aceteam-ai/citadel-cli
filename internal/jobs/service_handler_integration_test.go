//go:build integration

// Docker-gated integration test for the SERVICE_START port-binding fix
// (citadel-cli#415). Unlike service_handler_test.go (pure parse, no Docker),
// this drives ServiceHandler.serviceStart against a REAL docker daemon and
// asserts the started container actually has the expected HOST port published
// -- the exact property that was missing in #415, where the container came up
// with NetworkSettings.Ports == {} and the provisioned endpoint was unreachable.
//
// Build-tagged `integration` so it never runs in normal CI, and it t.Skips when
// docker is unavailable, so a developer without Docker sees no spurious failure.
//
// Run it with a working docker on PATH:
//
//	go test -tags integration -run TestServiceStartPublishesHostPort ./internal/jobs/ -v
package jobs

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// requireDocker skips the test unless a working docker CLI + daemon is present.
func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH; skipping SERVICE_START integration test")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skipf("docker daemon not reachable (%v); skipping SERVICE_START integration test", err)
	}
}

// TestServiceStartPublishesHostPort is the #415 regression: after SERVICE_START,
// the container must have the compose-declared host port published, and the
// result must report that reachable endpoint. It uses a tiny always-available
// image (busybox sleeping) with a fixed host port so the generic start->publish
// ->inspect path is exercised without a GPU or the diffusers image.
func TestServiceStartPublishesHostPort(t *testing.T) {
	requireDocker(t)

	const (
		svcName  = "porttest"
		hostPort = "7861" // matches the fixed diffusers host port
	)
	containerName := "citadel-" + svcName

	// Ensure a clean slate and always clean up (a foreign/leftover container with
	// the same name is exactly the #415 stale-container scenario).
	rm := func() { _ = exec.Command("docker", "rm", "-f", containerName).Run() }
	rm()
	t.Cleanup(rm)

	configDir := t.TempDir()
	servicesDir := filepath.Join(configDir, "services")
	if err := os.MkdirAll(servicesDir, 0o755); err != nil {
		t.Fatalf("mkdir services: %v", err)
	}
	compose := `services:
  ` + svcName + `:
    image: busybox:latest
    container_name: ` + containerName + `
    command: ["sleep", "3600"]
    ports:
      - "` + hostPort + `:80"
`
	composePath := filepath.Join(servicesDir, svcName+".yml")
	if err := os.WriteFile(composePath, []byte(compose), 0o600); err != nil {
		t.Fatalf("write compose: %v", err)
	}

	h := NewServiceHandler(configDir)
	svc := manifestService{
		Name:        svcName,
		Type:        "docker",
		ComposeFile: "services/" + svcName + ".yml",
	}

	out, err := h.serviceStart(JobContext{}, svc, "", 0)
	if err != nil {
		t.Fatalf("serviceStart returned error: %v", err)
	}
	var res serviceResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("serviceStart reported failure: %s", res.Error)
	}
	if !res.Running {
		t.Fatalf("serviceStart did not report the service running: %+v", res)
	}
	// #415 core assertion: the endpoint must be reported and carry the host port.
	if res.Endpoint == "" {
		t.Fatalf("serviceStart did not report a reachable endpoint (the #415 bug: NetworkSettings.Ports was empty)")
	}
	wantSuffix := ":" + hostPort
	if got := res.Endpoint; got[len(got)-len(wantSuffix):] != wantSuffix {
		t.Errorf("endpoint = %q, want host port %s", got, hostPort)
	}

	// Independently confirm via docker inspect that the host binding is real.
	inspect, err := exec.Command("docker", "inspect",
		"--format", "{{json .NetworkSettings.Ports}}", containerName).Output()
	if err != nil {
		t.Fatalf("docker inspect: %v", err)
	}
	if endpoint := firstPublishedHostEndpoint(inspect); endpoint == "" {
		t.Errorf("container %s has no published host port; NetworkSettings.Ports=%s", containerName, inspect)
	}
}
